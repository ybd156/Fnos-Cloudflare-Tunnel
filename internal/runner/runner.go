// Package runner supervises one cloudflared child process per tunnel: it starts
// and stops processes, restarts crashed ones with exponential backoff, and owns
// the runtime record the UI reads.
//
// The rule that keeps the record honest is that only the *current* child may
// touch it. A process on its way out must not clobber the state of the one that
// replaced it. The previous implementation got that ordering for free from a
// single-threaded event loop; here the log readers and the exit watcher run in
// their own goroutines, so "current" is a pointer comparison made under the
// manager lock and every field of a runtime record is read and written under
// that same lock.
package runner

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/cf"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

// Runtime status values. These strings are part of the API the front end polls,
// so they match the previous implementation verbatim.
const (
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopping = "stopping"
	StatusError    = "error"
)

// DNS check states recorded on the runtime record.
const (
	dnsStatusChecking = "checking"
	dnsStatusOK       = "ok"
	dnsStatusError    = "error"
	dnsStatusSkipped  = "skipped"
)

// errTunnelMissing is the message the UI shows for a tunnel that is not in the
// configuration (an id that was deleted in another tab, or a typo).
var errTunnelMissing = errors.New("隧道不存在")

// defaultLogLimit mirrors the default the log endpoint used.
const defaultLogLimit = 300

// cfRunner is the slice of *cf.Runner the supervisor needs. Depending on the
// interface rather than the concrete type keeps the tests free of a real
// cloudflared binary and of the account/DNS machinery behind it.
type cfRunner interface {
	BuildArgs(t config.Tunnel, s config.Settings) ([]string, map[string]string, error)
	Env(extra map[string]string) []string
	ResolveCredentials(t config.Tunnel) string
	WriteTunnelConfig(t config.Tunnel, credentialsFile string) (string, error)
}

// dnsResolver is the slice of *dnsroute.Resolver the supervisor needs.
type dnsResolver interface {
	Ensure(t config.Tunnel) dnsroute.Result
	Hostnames(t config.Tunnel) []string
	CacheFresh(name, hostname string) bool
}

// Manager owns every runtime record and every cloudflared child.
type Manager struct {
	p     paths.Paths
	log   *applog.Logger
	store *config.Store
	cf    cfRunner
	dns   dnsResolver

	spawn spawnFunc
	now   func() time.Time

	// Policy knobs, initialised from paths so tests can compress the clock.
	baseDelay   time.Duration
	maxDelay    time.Duration
	maxAttempts int
	stableAfter time.Duration
	startGrace  time.Duration
	killGrace   time.Duration
	pendingWait time.Duration
	drainGrace  time.Duration

	// startMu serialises whole Start calls. It is always taken before mu, never
	// the other way round, so the two locks cannot deadlock.
	startMu sync.Mutex

	mu   sync.Mutex
	recs map[string]*rec
	// live counts children that have been spawned and not yet reaped. WaitIdle
	// watches it.
	live int
	// idle is closed and replaced whenever a child exits.
	idle chan struct{}
}

// rec is the runtime record for one tunnel. Every field is guarded by
// Manager.mu.
type rec struct {
	status    string
	pid       int // 0 means "no process"
	startedAt string
	stoppedAt string
	url       string
	errMsg    string
	log       ring

	child    *run
	stopping bool
	attempts int

	restartTimer *time.Timer
	restartSeq   uint64

	lastExitCode *int
	lastExitAt   string

	dnsStatus  string
	dnsResults *DNSResults
	dnsMessage string
	dnsRunning bool

	// pendingRestart records that a graceful stop is in flight and the tunnel
	// should be launched again once the old child is really gone.
	pendingRestart bool
}

// run is one attempt at running a tunnel: the process plus the channel that
// tells the "start again once the old one is gone" path that it has exited.
type run struct {
	proc childProcess
	done chan struct{}
	pid  int
}

// New returns a Manager that supervises tunnels from store, using runner to
// build cloudflared invocations and dns to keep the CNAME routes in order.
func New(p paths.Paths, log *applog.Logger, store *config.Store, runner *cf.Runner, dns *dnsroute.Resolver) *Manager {
	return newManager(p, log, store, runner, dns)
}

func newManager(p paths.Paths, log *applog.Logger, store *config.Store, cfr cfRunner, dnsr dnsResolver) *Manager {
	return &Manager{
		p:     p,
		log:   log,
		store: store,
		cf:    cfr,
		dns:   dnsr,

		spawn: spawnExec,
		now:   time.Now,

		baseDelay:   time.Duration(paths.RestartBaseMS) * time.Millisecond,
		maxDelay:    time.Duration(paths.RestartMaxMS) * time.Millisecond,
		maxAttempts: paths.RestartMaxAttempts,
		stableAfter: time.Duration(paths.RestartStableMS) * time.Millisecond,
		startGrace:  8 * time.Second,
		killGrace:   8 * time.Second,
		pendingWait: 10 * time.Second,
		drainGrace:  time.Second,

		recs: make(map[string]*rec),
		idle: make(chan struct{}),
	}
}

/* --------------------------------------------------------------- lifecycle */

// Start launches the tunnel's process. A tunnel that is already running is left
// alone; a tunnel whose previous process is still shutting down is queued and
// launched as soon as that process exits.
func (m *Manager) Start(id string) error { return m.start(id, false) }

func (m *Manager) start(id string, keepAttempts bool) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	tunnel, ok := m.store.Tunnel(id)
	if !ok {
		return errTunnelMissing
	}

	m.mu.Lock()
	r := m.recordLocked(id)

	if r.child != nil && !r.stopping {
		// Already running: the previous implementation answered ok+already.
		m.mu.Unlock()
		return nil
	}

	if r.child != nil {
		/* The old process is on its way out (Stop sent SIGTERM a moment ago;
		 * cloudflared usually needs one to three seconds). Detach it from the
		 * record and launch once it is really gone, otherwise the same tunnel
		 * would briefly run two cloudflared processes: duplicate connections,
		 * and nobody reaps the old one. */
		prev := r.child
		r.child = nil
		r.pid = 0
		r.status = StatusStarting
		r.pendingRestart = true
		m.mu.Unlock()

		go func() {
			timer := time.NewTimer(m.pendingWait)
			defer timer.Stop()
			select {
			case <-prev.done:
			case <-timer.C:
				// SIGKILL after the stop grace period means exit should always
				// arrive; if it somehow does not, do not leave the tunnel stuck
				// on "starting".
			}
			m.mu.Lock()
			if !r.pendingRestart {
				// The user pressed stop while we were waiting.
				m.mu.Unlock()
				return
			}
			r.pendingRestart = false
			m.mu.Unlock()
			_ = m.start(id, keepAttempts)
		}()
		return nil
	}
	m.mu.Unlock()

	if !fileExists(m.p.CFBin) {
		msg := "cloudflared 可执行文件不存在: " + m.p.CFBin
		m.mu.Lock()
		r.status = StatusError
		r.errMsg = msg
		m.mu.Unlock()
		return errors.New(msg)
	}

	args, env, err := m.cf.BuildArgs(tunnel, m.store.Settings())
	if err != nil {
		m.mu.Lock()
		r.status = StatusError
		r.errMsg = err.Error()
		m.mu.Unlock()
		return err
	}

	m.mu.Lock()
	if !keepAttempts {
		r.attempts = 0
	}
	r.stopping = false
	r.status = StatusStarting
	r.errMsg = ""
	r.url = ""
	r.log.reset()
	r.startedAt = m.stampISO()
	m.mu.Unlock()

	m.info("starting tunnel " + id + " (" + tunnel.Type + "): " + m.p.CFBin + " " + strings.Join(args, " "))

	proc, err := m.spawn(m.p.CFBin, args, m.cf.Env(env), m.p.PkgVar)
	if err != nil {
		msg := "启动失败: " + err.Error()
		m.mu.Lock()
		r.status = StatusError
		r.errMsg = msg
		m.mu.Unlock()
		return errors.New(msg)
	}

	rn := &run{proc: proc, done: make(chan struct{}), pid: proc.PID()}

	m.mu.Lock()
	r.child = rn
	r.pid = rn.pid
	r.lastExitCode = nil
	m.live++
	/* A Stop that landed while the process was being spawned saw no child and
	 * returned; the child is real now, so that stop has to be honoured rather
	 * than leaving a running process the UI reports as stopped. */
	stopRequested := r.stopping
	if stopRequested {
		r.status = StatusStopping
	}
	m.mu.Unlock()

	go m.supervise(id, r, rn)

	if stopRequested {
		m.info("stopping tunnel " + id + " (pid " + strconv.Itoa(rn.pid) + ")")
		_ = proc.Terminate()
		time.AfterFunc(m.killGrace, func() {
			m.mu.Lock()
			current := r.child == rn
			m.mu.Unlock()
			if current {
				_ = proc.Kill()
			}
		})
		return nil
	}

	/* cloudflared can take a few seconds before it reports a connection. Keep
	 * the UI honest by promoting "starting" to "running" after a short grace
	 * period when the process is still alive and no fatal line was seen. */
	time.AfterFunc(m.startGrace, func() {
		m.mu.Lock()
		if r.child == rn && r.status == StatusStarting {
			r.status = StatusRunning
		}
		m.mu.Unlock()
	})

	// 本地管理隧道：顺便把 ingress 里的域名 CNAME 到隧道（幂等，失败不影响启动）。
	m.RunDNSRoutes(id, false)

	return nil
}

// supervise drains both output pipes and then records the exit.
//
// The exit is recorded only after the readers have finished: the pipes usually
// still hold the lines the child wrote just before it died, and those are
// exactly the lines that explain why it died. Waiting is bounded, so a
// grandchild that keeps a pipe open cannot stall the supervisor.
func (m *Manager) supervise(id string, r *rec, rn *run) {
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		m.drain(id, r, rn, rn.proc.Stdout())
	}()
	go func() {
		defer readers.Done()
		m.drain(id, r, rn, rn.proc.Stderr())
	}()

	info := rn.proc.Wait()

	drained := make(chan struct{})
	go func() {
		readers.Wait()
		close(drained)
	}()
	timer := time.NewTimer(m.drainGrace)
	select {
	case <-drained:
		timer.Stop()
	case <-timer.C:
	}

	close(rn.done)
	m.onExit(id, r, rn, info)
}

// drain copies one output pipe into the record. Output that arrives after the
// child stopped being the current one is dropped: it belongs to a process that
// has already been replaced.
func (m *Manager) drain(id string, r *rec, rn *run, rc io.Reader) {
	if rc == nil {
		return
	}
	if c, ok := rc.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			m.pushLogFrom(id, r, rn, string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func (m *Manager) onExit(id string, r *rec, rn *run, info exitInfo) {
	m.mu.Lock()
	m.live--
	m.signalIdleLocked()

	if r.child != rn {
		// A process that has been replaced must not touch the new record.
		m.mu.Unlock()
		m.info("tunnel " + id + ": superseded process " + strconv.Itoa(rn.pid) +
			" exited (code=" + codeText(info) + "), ignoring")
		return
	}

	wasStopping := r.stopping
	r.child = nil
	r.pid = 0
	r.lastExitCode = codePtr(info)
	now := m.stampISO()
	r.lastExitAt = now
	r.stoppedAt = now

	m.info("tunnel " + id + " exited code=" + codeText(info) +
		" signal=" + signalText(info) + " stopping=" + strconv.FormatBool(wasStopping))

	if wasStopping {
		r.status = StatusStopped
		r.errMsg = ""
		m.mu.Unlock()
		return
	}
	if info.Code == 0 && !info.Signaled {
		r.status = StatusStopped
	} else {
		r.status = StatusError
		if r.errMsg == "" {
			r.errMsg = exitMessage(info)
		}
	}
	m.mu.Unlock()

	m.scheduleRestart(id)
}

// Stop ends the tunnel's process: SIGTERM now, SIGKILL if it is still there
// after the grace period.
func (m *Manager) Stop(id string) {
	if !m.known(id) {
		// An unknown id must not leave a record behind (invariant 10).
		return
	}

	m.mu.Lock()
	r := m.recordLocked(id)

	if r.restartTimer != nil {
		r.restartTimer.Stop()
		r.restartTimer = nil
		r.restartSeq++
	}
	r.attempts = 0
	r.stopping = true
	r.pendingRestart = false

	child := r.child
	pid := r.pid
	if child == nil || pid == 0 {
		r.status = StatusStopped
		r.pid = 0
		m.mu.Unlock()
		return
	}

	// Set the state before signalling: once SIGTERM is out the exit watcher may
	// run at any moment, and it must not be overwritten with "stopping" after
	// it has already recorded "stopped".
	r.status = StatusStopping
	m.info("stopping tunnel " + id + " (pid " + strconv.Itoa(pid) + ")")
	_ = child.proc.Terminate()
	m.mu.Unlock()

	time.AfterFunc(m.killGrace, func() {
		m.mu.Lock()
		current := r.child == child && r.pid == pid
		m.mu.Unlock()
		if current {
			_ = child.proc.Kill()
		}
	})
}

// StopAll stops every tunnel in the configuration. Used on shutdown.
func (m *Manager) StopAll() {
	for _, tunnel := range m.store.Tunnels() {
		m.Stop(tunnel.ID)
	}
}

// Forget drops a tunnel's runtime record once the tunnel itself is gone. The
// record registry belongs to this package, so callers must not reach into it;
// a forgotten tunnel must also lose its restart timer, or the supervisor would
// keep resurrecting a tunnel that no longer exists.
func (m *Manager) Forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return
	}
	if r.restartTimer != nil {
		r.restartTimer.Stop()
		r.restartTimer = nil
	}
	r.restartSeq++
	r.pendingRestart = false
	delete(m.recs, id)
}

// WaitIdle blocks until no child is left, or until the timeout expires.
func (m *Manager) WaitIdle(timeout time.Duration) {
	deadline := m.now().Add(timeout)
	for {
		m.mu.Lock()
		n := m.live
		sig := m.idle
		m.mu.Unlock()
		if n == 0 {
			return
		}
		remaining := deadline.Sub(m.now())
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-sig:
			timer.Stop()
		case <-timer.C:
			return
		}
	}
}

/* ----------------------------------------------------------------- restart */

// scheduleRestart arms the exponential-backoff restart, if the tunnel and the
// global settings both allow it.
func (m *Manager) scheduleRestart(id string) {
	tunnel, ok := m.store.Tunnel(id)
	if !ok {
		return
	}
	settings := m.store.Settings()
	// The previous implementation also tested `tunnel.autoRestart === false`,
	// but normalizeTunnel never stored that field, so the test could not fire;
	// the tunnel-level switch that actually exists is `enabled`.
	if !settings.AutoRestart {
		return
	}
	if !tunnel.Enabled {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r := m.recordLocked(id)

	// A tunnel that ran long enough is not in a crash loop any more: the next
	// failure is a new problem and starts again from the first backoff step.
	if r.startedAt != "" {
		if started, err := time.Parse(time.RFC3339, r.startedAt); err == nil {
			if m.now().Sub(started) > m.stableAfter {
				r.attempts = 0
			}
		}
	}

	if r.attempts >= m.maxAttempts {
		r.status = StatusError
		r.errMsg = fmt.Sprintf("连续 %d 次启动失败，已停止自动重试", m.maxAttempts)
		m.warn("tunnel " + id + ": giving up after " + strconv.Itoa(m.maxAttempts) + " attempts")
		return
	}

	r.attempts++
	delay := backoffDelay(r.attempts, m.baseDelay, m.maxDelay)
	m.pushLogLocked(id, r, "将在 "+strconv.Itoa(int(math.Round(delay.Seconds())))+
		" 秒后自动重启（第 "+strconv.Itoa(r.attempts)+" 次）")

	r.restartSeq++
	seq := r.restartSeq
	if r.restartTimer != nil {
		r.restartTimer.Stop()
	}
	r.restartTimer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		if r.restartSeq != seq {
			// Cancelled or replaced while this timer was pending.
			m.mu.Unlock()
			return
		}
		r.restartTimer = nil
		r.status = StatusStarting
		m.mu.Unlock()
		_ = m.start(id, true)
	})
}

// backoffDelay is the restart delay after the given attempt: base doubled per
// attempt, capped at max. Attempt 1 waits base, attempt 2 waits 2*base, and so
// on.
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		return max
	}
	return delay
}

/* ---------------------------------------------------------------- internals */

// recordLocked returns the record for id, creating the default one on first
// use. Manager.mu must be held.
func (m *Manager) recordLocked(id string) *rec {
	if r, ok := m.recs[id]; ok {
		return r
	}
	r := &rec{status: StatusStopped}
	m.recs[id] = r
	return r
}

// known reports whether id names a tunnel this supervisor should act on: either
// it already has a record, or it is still in the configuration.
func (m *Manager) known(id string) bool {
	m.mu.Lock()
	_, has := m.recs[id]
	m.mu.Unlock()
	if has {
		return true
	}
	_, ok := m.store.Tunnel(id)
	return ok
}

func (m *Manager) signalIdleLocked() {
	close(m.idle)
	m.idle = make(chan struct{})
}

func (m *Manager) stampISO() string {
	return m.now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func (m *Manager) info(msg string) {
	if m.log != nil {
		m.log.Info(msg)
	}
}

func (m *Manager) warn(msg string) {
	if m.log != nil {
		m.log.Warn(msg)
	}
}

func fileExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

func codePtr(info exitInfo) *int {
	if info.Signaled || info.Code < 0 {
		return nil
	}
	code := info.Code
	return &code
}

func codeText(info exitInfo) string {
	if info.Signaled || info.Code < 0 {
		return "null"
	}
	return strconv.Itoa(info.Code)
}

func signalText(info exitInfo) string {
	if info.Signaled {
		// Go's portable process API reports "killed by a signal" without
		// naming it, so the service log says so rather than inventing a name.
		return "unknown"
	}
	return "none"
}

// exitMessage is the reason shown when cloudflared went away on its own.
func exitMessage(info exitInfo) string {
	if info.Signaled {
		return "cloudflared 退出，进程被信号终止"
	}
	if info.Code < 0 {
		return "cloudflared 退出，退出码未知"
	}
	return "cloudflared 退出，退出码 " + strconv.Itoa(info.Code)
}
