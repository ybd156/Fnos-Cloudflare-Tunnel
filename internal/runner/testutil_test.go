package runner

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

/* ------------------------------------------------------------------ harness */

type harness struct {
	m     *Manager
	p     paths.Paths
	log   *applog.Logger
	store *config.Store
	cf    *fakeCF
	dns   *fakeDNS
	sp    *stubSpawner
}

// testPaths builds a self-contained directory tree, so nothing the supervisor
// writes can escape into the real installation.
func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	root := t.TempDir()
	return paths.Paths{
		AppName:    "cloudflared",
		PkgVar:     root,
		PkgTmp:     filepath.Join(root, "tmp"),
		PkgEtc:     filepath.Join(root, "etc"),
		CFBin:      filepath.Join(root, "bin", "cloudflared"),
		CFHome:     root,
		CFDir:      filepath.Join(root, ".cloudflared"),
		CertFile:   filepath.Join(root, ".cloudflared", "cert.pem"),
		TunnelsDir: filepath.Join(root, "tunnels"),
		LogsDir:    filepath.Join(root, "logs"),
		ConfigFile: filepath.Join(root, "config.json"),
		AppLog:     filepath.Join(root, "server.log"),
		TCPHost:    "127.0.0.1",
	}
}

// newHarness wires a Manager to stub collaborators and a compressed clock, so
// the restart policy can be exercised in milliseconds instead of minutes.
func newHarness(t *testing.T, tunnels ...config.Tunnel) *harness {
	t.Helper()

	p := testPaths(t)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.CFBin), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(p.CFBin, []byte("stub"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	// The logger's console sinks are silenced: tests should not print, but the
	// per-tunnel log files still have to be written for the tail-read test.
	lg := applog.New(p.AppLog)
	lg.Out = nil
	lg.Err = nil

	store := config.NewStore(p.ConfigFile, lg)
	store.Load()
	for _, tunnel := range tunnels {
		store.Add(tunnel)
	}

	fc := &fakeCF{}
	fd := &fakeDNS{}
	sp := &stubSpawner{}

	m := newManager(p, lg, store, fc, fd)
	m.spawn = sp.spawn
	m.baseDelay = 10 * time.Millisecond
	m.maxDelay = 40 * time.Millisecond
	m.stableAfter = 50 * time.Millisecond
	m.startGrace = 20 * time.Millisecond
	m.killGrace = 30 * time.Millisecond
	m.pendingWait = 300 * time.Millisecond

	// Leave no goroutine, timer or open log file behind: an open server.log
	// keeps the directory busy and makes TempDir cleanup fail.
	t.Cleanup(func() {
		for i := 0; i < 3; i++ {
			m.StopAll()
			sp.closeAll()
			m.WaitIdle(2 * time.Second)
		}
		time.Sleep(20 * time.Millisecond)
		lg.Close()
	})

	return &harness{m: m, p: p, log: lg, store: store, cf: fc, dns: fd, sp: sp}
}

// withRec runs fn with the manager lock held, which is the only way a test may
// touch a runtime record.
func (h *harness) withRec(id string, fn func(*rec)) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	fn(h.m.recordLocked(id))
}

func (h *harness) status(id string) string {
	var s string
	h.withRec(id, func(r *rec) { s = r.status })
	return s
}

func (h *harness) hasRecord(id string) bool {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	_, ok := h.m.recs[id]
	return ok
}

func testTunnel(id string) config.Tunnel {
	return config.Tunnel{
		ID:         id,
		Name:       "tunnel " + id,
		Type:       config.TypeLocal,
		Enabled:    true,
		AutoStart:  true,
		AutoDNS:    true,
		TunnelName: "edge-" + id,
		CatchAll:   "http_status:404",
		Ingress:    []config.Ingress{},
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

/* ------------------------------------------------------------ fake cloudflared */

type fakeCF struct {
	mu sync.Mutex

	buildErr error
	args     []string
	extraEnv map[string]string

	cred         string
	writeErr     error
	writes       int
	lastSettings config.Settings
	builds       int
}

func (f *fakeCF) BuildArgs(t config.Tunnel, s config.Settings) ([]string, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds++
	f.lastSettings = s
	if f.buildErr != nil {
		return nil, nil, f.buildErr
	}
	if f.args != nil {
		return f.args, f.extraEnv, nil
	}
	return []string{"tunnel", "--no-autoupdate", "run"}, nil, nil
}

func (f *fakeCF) Env(extra map[string]string) []string { return []string{"HOME=/tmp/fncfd"} }

func (f *fakeCF) ResolveCredentials(t config.Tunnel) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cred
}

func (f *fakeCF) WriteTunnelConfig(t config.Tunnel, credentialsFile string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	return filepath.Join("/tmp", t.ID, "config.yml"), f.writeErr
}

func (f *fakeCF) setBuildErr(err error) {
	f.mu.Lock()
	f.buildErr = err
	f.mu.Unlock()
}

func (f *fakeCF) setCred(cred string) {
	f.mu.Lock()
	f.cred = cred
	f.mu.Unlock()
}

func (f *fakeCF) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *fakeCF) setWriteErr(err error) {
	f.mu.Lock()
	f.writeErr = err
	f.mu.Unlock()
}

// settingsSeen returns the settings the supervisor last built args with.
func (f *fakeCF) settingsSeen() (config.Settings, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSettings, f.builds
}

/* ---------------------------------------------------------------- fake DNS */

type fakeDNS struct {
	mu sync.Mutex

	hostnames []string
	fresh     map[string]bool
	result    dnsroute.Result
	panicErr  error
	calls     int
	gate      chan struct{}
}

func (f *fakeDNS) Ensure(t config.Tunnel) dnsroute.Result {
	f.mu.Lock()
	f.calls++
	gate := f.gate
	result := f.result
	panicErr := f.panicErr
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if panicErr != nil {
		panic(panicErr)
	}
	return result
}

func (f *fakeDNS) Hostnames(t config.Tunnel) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hostnames...)
}

func (f *fakeDNS) CacheFresh(name, hostname string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fresh[hostname]
}

func (f *fakeDNS) set(hostnames []string, result dnsroute.Result) {
	f.mu.Lock()
	f.hostnames = hostnames
	f.result = result
	f.mu.Unlock()
}

func (f *fakeDNS) setFresh(fresh bool) {
	f.mu.Lock()
	f.fresh = map[string]bool{}
	for _, h := range f.hostnames {
		f.fresh[h] = fresh
	}
	f.mu.Unlock()
}

func (f *fakeDNS) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeDNS) setPanic(err error) {
	f.mu.Lock()
	f.panicErr = err
	f.mu.Unlock()
}

func (f *fakeDNS) setGate(gate chan struct{}) {
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
}

/* ------------------------------------------------------------- stub process */

type stubSpawner struct {
	mu       sync.Mutex
	spawned  int
	fail     error
	children []*stubChild
}

func (s *stubSpawner) spawn(bin string, args, env []string, dir string) (childProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	child := newStubChild(s.spawned + 1)
	s.spawned++
	s.children = append(s.children, child)
	return child, nil
}

func (s *stubSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawned
}

// closeAll ends every child the test started, including ones the manager has
// already detached. Used by cleanup so no stub process outlives its test.
func (s *stubSpawner) closeAll() {
	s.mu.Lock()
	children := append([]*stubChild(nil), s.children...)
	s.mu.Unlock()
	for _, c := range children {
		c.exit(exitInfo{Code: 0})
	}
}

func (s *stubSpawner) child(i int) *stubChild {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.children[i]
}

func (s *stubSpawner) last() *stubChild {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.children) == 0 {
		return nil
	}
	return s.children[len(s.children)-1]
}

// stubChild is a scripted cloudflared: it produces whatever output the test
// writes into its pipes, and exits when the test says so (or, optionally, when
// it is asked to terminate).
type stubChild struct {
	pid int

	outR *io.PipeReader
	outW *io.PipeWriter
	errR *io.PipeReader
	errW *io.PipeWriter

	exitCh chan exitInfo

	closeOnce sync.Once
	termOnce  sync.Once
	killOnce  sync.Once

	terminated chan struct{}
	killed     chan struct{}

	// exitOnTerminate makes Terminate end the process the way a well-behaved
	// cloudflared does.
	exitOnTerminate bool
}

func newStubChild(pid int) *stubChild {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	return &stubChild{
		pid:        pid,
		outR:       outR,
		outW:       outW,
		errR:       errR,
		errW:       errW,
		exitCh:     make(chan exitInfo, 1),
		terminated: make(chan struct{}),
		killed:     make(chan struct{}),
	}
}

func (c *stubChild) PID() int          { return c.pid }
func (c *stubChild) Stdout() io.Reader { return c.outR }
func (c *stubChild) Stderr() io.Reader { return c.errR }

func (c *stubChild) Wait() exitInfo { return <-c.exitCh }

func (c *stubChild) Terminate() error {
	c.termOnce.Do(func() { close(c.terminated) })
	if c.exitOnTerminate {
		c.exit(exitInfo{Code: 0})
	}
	return nil
}

func (c *stubChild) Kill() error {
	c.killOnce.Do(func() { close(c.killed) })
	c.exit(exitInfo{Code: -1, Signaled: true})
	return nil
}

// writeOutput feeds the child's stdout. It never blocks the test: the read side
// may already be gone.
func (c *stubChild) writeOutput(s string) {
	go func() { _, _ = c.outW.Write([]byte(s)) }()
}

func (c *stubChild) exit(info exitInfo) {
	c.closeOnce.Do(func() {
		// Closing the write ends is what makes the supervisor's readers see EOF
		// and stop, exactly as a real process exit does.
		_ = c.outW.Close()
		_ = c.errW.Close()
		select {
		case c.exitCh <- info:
		default:
		}
	})
}

func (c *stubChild) isTerminated() bool {
	select {
	case <-c.terminated:
		return true
	default:
		return false
	}
}

func (c *stubChild) isKilled() bool {
	select {
	case <-c.killed:
		return true
	default:
		return false
	}
}
