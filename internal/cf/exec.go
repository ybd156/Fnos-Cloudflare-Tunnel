// Package cf wraps the upstream cloudflared binary: one-shot commands, the
// argv/config.yml for a supervised tunnel, the account workflow and the binary
// updater.
//
// Nothing here supervises a process -- that is internal/runner's job. Every
// function that returns an error returns a message that can be shown to the
// user verbatim, in the same words the previous implementation used.
package cf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

const (
	// maxCapture bounds how much of a child's stdout/stderr is kept. The
	// previous implementation passed the same 8 MB ceiling to execFile; here it
	// only truncates, because losing the tail of a chatty command is a much
	// better outcome than allocating without bound.
	maxCapture = 8 << 20
	// defaultTimeout matches the 30 s default of the previous implementation.
	defaultTimeout = 30 * time.Second
)

// ExecOptions are the per-command knobs.
type ExecOptions struct {
	Timeout time.Duration
	Env     map[string]string
	Dir     string
}

// ExecResult is what a finished command produced. Err is the failure text of
// the command itself (empty on success); Code is 0 on success.
type ExecResult struct {
	Code   int
	Stdout string
	Stderr string
	Err    string
}

// Runner shells out to the bundled cloudflared binary.
type Runner struct {
	p   paths.Paths
	log *applog.Logger

	mu            sync.Mutex
	cachedVersion string
	// execFn is a test seam: nil means "really run the binary".
	execFn func(args []string, opts ExecOptions) ExecResult

	loginMu sync.Mutex
	login   loginState
}

// New returns a Runner for the payload described by p.
func New(p paths.Paths, log *applog.Logger) *Runner {
	return &Runner{p: p, log: log, login: loginState{status: "idle"}}
}

// Env returns the child environment: the service's own environment with the
// three variables cloudflared must see (HOME, TUNNEL_ORIGIN_CERT,
// NO_AUTOUPDATE) and then extra applied on top.
//
// HOME is redirected because cloudflared keeps its state in $HOME/.cloudflared;
// pointing it at the app's var directory keeps credentials inside the package.
func (r *Runner) Env(extra map[string]string) []string {
	vals := make(map[string]string, len(os.Environ())+4+len(extra))
	order := make([]string, 0, len(os.Environ())+4+len(extra))

	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k, v := kv[:i], kv[i+1:]
		if _, dup := vals[k]; !dup {
			order = append(order, k)
		}
		vals[k] = v
	}
	set := func(k, v string) {
		if _, ok := vals[k]; !ok {
			order = append(order, k)
		}
		vals[k] = v
	}

	set("HOME", r.p.CFHome)
	set("TUNNEL_ORIGIN_CERT", r.p.CertFile)
	set("NO_AUTOUPDATE", "true")

	// Sorted so the slice is deterministic (the map iteration order is not).
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		set(k, extra[k])
	}

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+vals[k])
	}
	return out
}

// Exec runs cloudflared once and collects its output.
//
// It never returns an error: callers inspect Code and Err so that a failed
// command becomes an API message instead of a crashed request handler.
func (r *Runner) Exec(args []string, opts ExecOptions) ExecResult {
	if r.execFn != nil {
		return r.execFn(args, opts)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	dir := opts.Dir
	if dir == "" {
		dir = r.p.PkgVar
	}

	cmd := exec.CommandContext(ctx, r.p.ActiveCFBin(), args...)
	cmd.Dir = dir
	cmd.Env = r.Env(opts.Env)

	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr == nil {
		return res
	}

	res.Code = 1
	if code := exitCode(runErr); code > 0 {
		res.Code = code
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Err = fmt.Sprintf("cloudflared 命令超时（%s）", timeout)
	} else {
		res.Err = runErr.Error()
	}
	if stdout.overflowed || stderr.overflowed {
		r.warn("cloudflared output truncated at", maxCapture, "bytes")
	}
	return res
}

// exitCode extracts a usable exit code, mirroring the previous
// implementation's "use err.code when it is a number, otherwise 1".
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 0
}

// cappedBuffer keeps at most maxCapture bytes and remembers that it had to
// drop the rest.
type cappedBuffer struct {
	buf        []byte
	overflowed bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := maxCapture - len(c.buf)
	if room <= 0 {
		if n > 0 {
			c.overflowed = true
		}
		return n, nil
	}
	if n > room {
		c.overflowed = true
		p = p[:room]
	}
	c.buf = append(c.buf, p...)
	return n, nil
}

func (c *cappedBuffer) String() string { return string(c.buf) }

var (
	// "cloudflared version 2024.1.0" -- the only shape the CLI prints.
	versionLineRE = regexp.MustCompile(`cloudflared version ([0-9][^\s]*)`)
	// Some builds print the bare version.
	versionBareRE = regexp.MustCompile(`^([0-9][^\s]*)`)
)

// Version reports the cloudflared version, cached unless refresh is set. It
// returns "" when the binary is missing, which the UI renders as "未安装".
func (r *Runner) Version(refresh bool) string {
	if !refresh {
		r.mu.Lock()
		cached := r.cachedVersion
		r.mu.Unlock()
		if cached != "" {
			return cached
		}
	}
	if !fsx.Exists(r.p.ActiveCFBin()) {
		return ""
	}

	res := r.Exec([]string{"--version"}, ExecOptions{Timeout: 10 * time.Second})
	text := strings.TrimSpace(res.Stdout + " " + res.Stderr)

	version := text
	if m := versionLineRE.FindStringSubmatch(text); m != nil {
		version = m[1]
	} else if m := versionBareRE.FindStringSubmatch(text); m != nil {
		version = m[1]
	} else if version == "" {
		version = "unknown"
	}

	r.mu.Lock()
	r.cachedVersion = version
	r.mu.Unlock()
	return version
}

// SplitArgs splits a user supplied extra-args string the way a shell would, so
// a value containing spaces can still be passed as one argument. Quotes are
// removed; an unterminated quote falls back to whitespace splitting, exactly
// like the regular expression the previous implementation used.
func SplitArgs(s string) []string {
	out := make([]string, 0, 4)
	i := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			i++
			continue
		case '"':
			if end := strings.IndexByte(s[i+1:], '"'); end >= 0 {
				out = append(out, s[i+1:i+1+end])
				i += end + 2
				continue
			}
		case '\'':
			if end := strings.IndexByte(s[i+1:], '\''); end >= 0 {
				out = append(out, s[i+1:i+1+end])
				i += end + 2
				continue
			}
		}
		// Unquoted run, or a quote with no partner: take the whole word.
		start := i
		for i < len(s) && !isSpace(s[i]) {
			i++
		}
		out = append(out, s[start:i])
	}
	return out
}

func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// GlobalArgs are the flags every cloudflared invocation gets.
func GlobalArgs(s config.Settings) []string {
	args := make([]string, 0, 12)
	if s.Protocol != "" && s.Protocol != "auto" {
		args = append(args, "--protocol", s.Protocol)
	}
	if s.EdgeIPVersion != "" {
		args = append(args, "--edge-ip-version", s.EdgeIPVersion)
	}
	if s.Region != "" {
		args = append(args, "--region", s.Region)
	}
	if s.LogLevel != "" {
		args = append(args, "--loglevel", s.LogLevel)
	}
	if s.MetricsPort != 0 {
		args = append(args, "--metrics", fmt.Sprintf("127.0.0.1:%d", s.MetricsPort))
	}
	args = append(args, "--no-autoupdate")
	// The settings page's "附加参数" used to be stored and never passed on.
	args = append(args, SplitArgs(s.ExtraArgs)...)
	return args
}

func (r *Runner) info(parts ...any) {
	if r.log != nil {
		r.log.Info(parts...)
	}
}

func (r *Runner) warn(parts ...any) {
	if r.log != nil {
		r.log.Warn(parts...)
	}
}
