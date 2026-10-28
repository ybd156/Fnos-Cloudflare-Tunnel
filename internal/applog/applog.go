// Package applog writes the service log and the per-tunnel logs.
//
// Two rules shape it, both learned from defects:
//
//  1. Nothing may grow without bound. Both the server log and the per-tunnel
//     logs rotate, so a chatty tunnel cannot fill the NAS system volume over
//     weeks.
//  2. The log endpoints read only a bounded tail, and decode only the bytes
//     actually read. Logs rotate underneath a reader, and decoding a buffer
//     sized from an earlier stat() emitted uninitialised memory as log text.
package applog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/fsx"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

// ISO layout used for the service log, matching the format the previous
// implementation produced: 2026-10-28T05:42:49.842Z
const isoLayout = "2006-01-02T15:04:05.000Z"

// sink is one open log file. Keeping the file open across writes avoids an
// open/close pair per line, which the previous implementation paid for every
// line cloudflared printed.
type sink struct {
	file    *os.File
	checked time.Time
	max     int64
}

// Logger writes the service log and any number of private log files.
type Logger struct {
	appLog string

	Out io.Writer
	Err io.Writer

	mu    sync.Mutex
	sinks map[string]*sink
}

// New returns a Logger whose service log is appLog.
func New(appLog string) *Logger {
	return &Logger{
		appLog: appLog,
		Out:    os.Stdout,
		Err:    os.Stderr,
		sinks:  make(map[string]*sink),
	}
}

// Info, Warn and Error write one line to the console and to the service log.
func (l *Logger) Info(parts ...any)  { l.log("INFO", parts...) }
func (l *Logger) Warn(parts ...any)  { l.log("WARN", parts...) }
func (l *Logger) Error(parts ...any) { l.log("ERROR", parts...) }

func (l *Logger) log(level string, parts ...any) {
	line := "[" + time.Now().UTC().Format(isoLayout) + "] [" + level + "] " + join(parts) + "\n"
	w := l.Out
	if level == "ERROR" {
		w = l.Err
	}
	if w != nil {
		_, _ = io.WriteString(w, line)
	}
	l.AppendPrivate(l.appLog, line, paths.MaxAppLogBytes)
}

// AppendPrivate appends one line to a log file that must not be world-readable,
// rotating it once it exceeds maxBytes. Failures are swallowed: a full disk must
// not take the service down.
func (l *Logger) AppendPrivate(file, line string, maxBytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	s, err := l.sinkFor(file, maxBytes)
	if err != nil {
		return
	}
	now := time.Now()
	if now.Sub(s.checked) > paths.LogRotateCheckMS*time.Millisecond {
		s.checked = now
		l.rotateLocked(file, s)
	}
	_, _ = io.WriteString(s.file, line)
}

func (l *Logger) sinkFor(file string, maxBytes int64) (*sink, error) {
	if s, ok := l.sinks[file]; ok {
		return s, nil
	}
	fsx.MkdirAll(filepath.Dir(file), 0o755)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	// This filesystem can hand out generous default modes for new files.
	fsx.ChmodQuiet(file, 0o640)
	// checked is left at the zero time on purpose: the first append then runs
	// the rotation check, so a log that grew while the service was down is
	// rotated on its first write instead of after the next 30-second tick.
	s := &sink{file: f, max: maxBytes}
	l.sinks[file] = s
	return s, nil
}

// rotateLocked renames the log aside once it passes its cap. One generation is
// kept, so a tunnel costs at most twice its cap on disk.
func (l *Logger) rotateLocked(name string, s *sink) {
	st, err := s.file.Stat()
	if err != nil || st.Size() <= s.max {
		return
	}
	_ = s.file.Close()
	delete(l.sinks, name)
	if err := os.Rename(name, name+".1"); err != nil {
		return
	}
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return
	}
	s.file = f
	l.sinks[name] = s
}

// RotateAppLogNow rotates the service log. Called at startup, the one moment no
// other writer can be active.
func (l *Logger) RotateAppLogNow() {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.sinkFor(l.appLog, paths.MaxAppLogBytes)
	if err != nil {
		return
	}
	s.max = paths.MaxAppLogBytes
	l.rotateLocked(l.appLog, s)
}

// Close releases every open log file.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, s := range l.sinks {
		_ = s.file.Close()
		delete(l.sinks, name)
	}
}

// ReadTailLines returns at most maxLines of the last lines of file, reading no
// more than maxBytes from the end.
//
// A byte window can begin mid-line and mid-UTF-8-sequence, so the first
// (partial) line is dropped. Only the bytes actually read are decoded: the file
// can shrink between Stat and Read because it rotates underneath us.
func ReadTailLines(file string, maxBytes int64, maxLines int) []string {
	st, err := os.Stat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return nil
	}
	start := st.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	length := st.Size() - start

	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()

	buf := make([]byte, length)
	n, err := f.ReadAt(buf, start)
	if n <= 0 || (err != nil && err != io.EOF) {
		return nil
	}
	text := string(buf[:n])
	if start > 0 {
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			text = text[nl+1:]
		} else {
			text = ""
		}
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			out = append(out, line)
		}
	}
	if maxLines > 0 && len(out) > maxLines {
		out = out[len(out)-maxLines:]
	}
	return out
}

// join renders log parts the way the previous implementation did: strings
// verbatim, everything else through its default formatting.
func join(parts []any) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch v := p.(type) {
		case string:
			b.WriteString(v)
		case error:
			b.WriteString(v.Error())
		default:
			b.WriteString(fmt.Sprint(v))
		}
	}
	return b.String()
}
