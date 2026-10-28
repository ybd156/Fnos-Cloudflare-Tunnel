package runner

import (
	"regexp"
	"strings"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

// Line patterns cloudflared emits. They are matched against the raw line, so
// the surrounding decoration (timestamp, level) does not matter.
var (
	// The URL a quick tunnel prints once it is up.
	quickURLRE = regexp.MustCompile(`https://[-a-z0-9]+\.trycloudflare\.com`)
	// Evidence that the tunnel has at least one connection registered.
	readyRE = regexp.MustCompile(`Registered tunnel connection|Connection [0-9a-f-]+ registered|Initial protocol`)
	// Lines worth quoting back to the user when a start is failing.
	errorRE = regexp.MustCompile(`(?i)ERR |failed to |Unable to |error=|level=error`)
)

// errorSampleRunes caps the quoted error line, as the previous implementation
// did with line.slice(0, 300).
const errorSampleRunes = 300

// ring is the bounded in-memory log of one tunnel: at most paths.MaxLogLines
// entries, oldest dropped first.
type ring struct {
	buf   []string
	start int // index of the oldest entry once buf is full
}

func (r *ring) reset() {
	r.buf = r.buf[:0]
	r.start = 0
}

func (r *ring) len() int { return len(r.buf) }

func (r *ring) push(entry string) {
	if len(r.buf) < paths.MaxLogLines {
		r.buf = append(r.buf, entry)
		return
	}
	r.buf[r.start] = entry
	r.start = (r.start + 1) % len(r.buf)
}

// tail returns the newest limit entries, oldest first.
func (r *ring) tail(limit int) []string {
	n := len(r.buf)
	if n == 0 {
		return nil
	}
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		idx := (r.start + len(r.buf) - n + i) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

// pushLog appends a chunk of child output to the record and to the tunnel log.
func (m *Manager) pushLog(id, text string) {
	m.mu.Lock()
	m.pushLogLocked(id, m.recordLocked(id), text)
	m.mu.Unlock()
}

// pushLogFrom appends output only if rn is still the current child. The check
// and the write share one critical section, so a process that is replaced
// between two reads cannot append to the record of its successor.
func (m *Manager) pushLogFrom(id string, r *rec, rn *run, text string) {
	m.mu.Lock()
	if r.child == rn {
		m.pushLogLocked(id, r, text)
	}
	m.mu.Unlock()
}

// pushLogLocked is pushLog for callers that already hold Manager.mu.
func (m *Manager) pushLogLocked(id string, r *rec, text string) {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	file := m.p.TunnelLog(id)
	for _, line := range lines {
		// A trailing newline produces one empty tail entry; keep a genuinely
		// empty line in the middle of a chunk.
		if line == "" && len(lines) > 1 {
			continue
		}
		entry := "[" + m.now().UTC().Format("15:04:05") + "] " + line
		r.log.push(entry)
		if m.log != nil {
			// The rotation cap is passed explicitly because AppendPrivate also
			// serves the server log, which has a different limit.
			m.log.AppendPrivate(file, entry+"\n", paths.MaxTunnelLogBytes)
		}
		m.observeLocked(id, r, line)
	}
}

// observeLocked folds what a line says into the runtime record: the quick URL,
// the transition to running, and the first error of a failing start.
func (m *Manager) observeLocked(id string, r *rec, line string) {
	if quick := quickURLRE.FindString(line); quick != "" {
		r.url = quick
		m.info("tunnel " + id + " quick URL: " + r.url)
	}
	if readyRE.MatchString(line) && r.status != StatusRunning {
		r.status = StatusRunning
		r.errMsg = ""
	}
	if errorRE.MatchString(line) && r.status == StatusStarting {
		r.errMsg = truncateRunes(line, errorSampleRunes)
	}
	// A tunnel can be perfectly "running" at the edge while every request it
	// forwards fails because the origin is down. That is invisible in the status
	// field, so it gets its own slot.
	m.noteOriginErrorLocked(r, line)
}

// Logs returns the tunnel's recent output: the in-memory ring first, and the
// bounded tail of the log file when the ring is empty (a tunnel that has not
// produced output since the service started).
func (m *Manager) Logs(id string, limit int) []string {
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > paths.MaxLogLines {
		limit = paths.MaxLogLines
	}
	// An unknown id must not leave a record behind (invariant 10).
	if !m.known(id) {
		return nil
	}

	m.mu.Lock()
	lines := m.recordLocked(id).log.tail(limit)
	m.mu.Unlock()
	if len(lines) > 0 {
		return lines
	}
	return applog.ReadTailLines(m.p.TunnelLog(id), paths.MaxTailBytes, limit)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
