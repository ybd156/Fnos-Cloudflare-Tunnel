package runner

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBackoffDelaySequence(t *testing.T) {
	base := 5 * time.Second
	max := 60 * time.Second

	want := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
		60 * time.Second,
		60 * time.Second,
	}
	for i, expected := range want {
		attempt := i + 1
		if got := backoffDelay(attempt, base, max); got != expected {
			t.Errorf("backoffDelay(%d) = %v, want %v", attempt, got, expected)
		}
	}

	// Degenerate inputs must stay bounded rather than overflow.
	if got := backoffDelay(0, base, max); got != base {
		t.Errorf("backoffDelay(0) = %v, want %v", got, base)
	}
	if got := backoffDelay(-3, base, max); got != base {
		t.Errorf("backoffDelay(-3) = %v, want %v", got, base)
	}
	if got := backoffDelay(200, base, max); got != max {
		t.Errorf("backoffDelay(200) = %v, want %v", got, max)
	}
}

func TestStartUnknownTunnel(t *testing.T) {
	h := newHarness(t)

	err := h.m.Start("nope")
	if err == nil || err.Error() != "隧道不存在" {
		t.Fatalf("Start(unknown) = %v, want 隧道不存在", err)
	}
	if h.hasRecord("nope") {
		t.Fatal("Start of an unknown tunnel must not create a runtime record")
	}
	if h.sp.count() != 0 {
		t.Fatalf("spawned %d children for an unknown tunnel", h.sp.count())
	}
}

func TestStartMissingBinary(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := removeFile(h.p.CFBin); err != nil {
		t.Fatalf("remove fake binary: %v", err)
	}

	err := h.m.Start("a")
	if err == nil {
		t.Fatal("Start with no cloudflared binary must fail")
	}
	want := "cloudflared 可执行文件不存在: " + h.p.CFBin
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if got := h.status("a"); got != StatusError {
		t.Fatalf("status = %q, want %q", got, StatusError)
	}
	var recorded string
	h.withRec("a", func(r *rec) { recorded = r.errMsg })
	if recorded != want {
		t.Fatalf("recorded error = %q, want %q", recorded, want)
	}
}

func TestStartBuildArgsFailure(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.cf.setBuildErr(errors.New("请填写隧道 Token"))

	if err := h.m.Start("a"); err == nil || err.Error() != "请填写隧道 Token" {
		t.Fatalf("Start = %v, want the BuildArgs message", err)
	}
	if got := h.status("a"); got != StatusError {
		t.Fatalf("status = %q, want %q", got, StatusError)
	}
}

func TestStartIsIdempotentWhileRunning(t *testing.T) {
	h := newHarness(t, testTunnel("a"))

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if got := h.sp.count(); got != 1 {
		t.Fatalf("spawned %d children, want 1", got)
	}
	if got := h.status("a"); got != StatusStarting {
		t.Fatalf("status = %q, want %q", got, StatusStarting)
	}
	if !h.m.Running("a") {
		t.Fatal("Running must report true while the child is alive")
	}
}

func TestStartingIsPromotedToRunningAfterGrace(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 2*time.Second, "the grace period to promote starting", func() bool {
		return h.status("a") == StatusRunning
	})
}

func TestExitSchedulesRestartWithBackoff(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	h.sp.last().exit(exitInfo{Code: 1})

	waitFor(t, 2*time.Second, "the exit to be recorded", func() bool {
		return h.status("a") == StatusError
	})
	var (
		attempts  int
		errMsg    string
		hasTimer  bool
		exitCode  *int
		lastExit  string
		stoppedAt string
	)
	h.withRec("a", func(r *rec) {
		attempts = r.attempts
		errMsg = r.errMsg
		hasTimer = r.restartTimer != nil
		exitCode = r.lastExitCode
		lastExit = r.lastExitAt
		stoppedAt = r.stoppedAt
	})
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if !hasTimer {
		t.Fatal("a restart timer should be armed")
	}
	if errMsg != "cloudflared 退出，退出码 1" {
		t.Fatalf("error = %q, want the exit-code message", errMsg)
	}
	if exitCode == nil || *exitCode != 1 {
		t.Fatalf("lastExitCode = %v, want 1", exitCode)
	}
	if lastExit == "" || stoppedAt != lastExit {
		t.Fatalf("lastExitAt = %q, stoppedAt = %q; both must be set to the same instant", lastExit, stoppedAt)
	}

	// The armed timer must really start the tunnel again.
	waitFor(t, 2*time.Second, "the scheduled restart to spawn a second child", func() bool {
		return h.sp.count() == 2
	})
}

func TestRestartGivesUpAfterMaxAttempts(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.withRec("a", func(r *rec) { r.attempts = h.m.maxAttempts })

	h.sp.last().exit(exitInfo{Code: 1})

	waitFor(t, 2*time.Second, "the give-up message", func() bool {
		return h.status("a") == StatusError
	})
	var errMsg string
	h.withRec("a", func(r *rec) { errMsg = r.errMsg })
	want := "连续 10 次启动失败，已停止自动重试"
	if errMsg != want {
		t.Fatalf("error = %q, want %q", errMsg, want)
	}
	if h.sp.count() != 1 {
		t.Fatalf("spawned %d children, want no further attempt", h.sp.count())
	}
}

func TestStableRunResetsAttempts(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Pretend the process had been up for well over the stable threshold.
	h.withRec("a", func(r *rec) {
		r.attempts = 5
		r.startedAt = time.Now().UTC().Add(-10 * time.Second).Format("2006-01-02T15:04:05.000Z")
	})

	h.sp.last().exit(exitInfo{Code: 1})

	waitFor(t, 2*time.Second, "the restart to be scheduled", func() bool {
		var armed bool
		h.withRec("a", func(r *rec) { armed = r.restartTimer != nil })
		return armed
	})
	var attempts int
	h.withRec("a", func(r *rec) { attempts = r.attempts })
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 after a stable run reset the counter", attempts)
	}
}

func TestExitWhileStoppingDoesNotRestart(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	child := h.sp.last()
	child.exitOnTerminate = true

	h.m.Stop("a")
	waitFor(t, 2*time.Second, "the process to exit", func() bool {
		return h.status("a") == StatusStopped
	})

	var (
		errMsg   string
		attempts int
		armed    bool
	)
	h.withRec("a", func(r *rec) {
		errMsg = r.errMsg
		attempts = r.attempts
		armed = r.restartTimer != nil
	})
	if errMsg != "" {
		t.Fatalf("error = %q, want empty for a clean stop", errMsg)
	}
	if attempts != 0 || armed {
		t.Fatalf("attempts = %d, timer = %v; a stopped tunnel must not restart", attempts, armed)
	}
}

func TestForgetClearsRestartTimer(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.m.baseDelay = 200 * time.Millisecond
	h.m.maxDelay = 200 * time.Millisecond

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.sp.last().exit(exitInfo{Code: 1})
	waitFor(t, 2*time.Second, "the restart timer", func() bool {
		var armed bool
		h.withRec("a", func(r *rec) { armed = r.restartTimer != nil })
		return armed
	})

	h.m.Forget("a")
	if h.hasRecord("a") {
		t.Fatal("Forget must drop the runtime record")
	}

	// The pending restart must not resurrect a tunnel that no longer exists.
	time.Sleep(400 * time.Millisecond)
	if got := h.sp.count(); got != 1 {
		t.Fatalf("spawned %d children after Forget, want 1", got)
	}
	if h.hasRecord("a") {
		t.Fatal("the cleared restart timer must not recreate the record")
	}
}

func TestPendingRestartWaitsForOldProcess(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := h.sp.last()

	// Stop sends SIGTERM; the stub keeps running, as cloudflared often does for
	// a second or two.
	h.m.Stop("a")
	if !first.isTerminated() {
		t.Fatal("Stop must send SIGTERM")
	}

	// A start during the graceful shutdown must queue, not spawn a second
	// process next to the dying one.
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start while stopping: %v", err)
	}
	var pending bool
	h.withRec("a", func(r *rec) {
		pending = r.pendingRestart
		if r.child != nil {
			t.Fatal("the dying process must be detached from the record")
		}
	})
	if !pending {
		t.Fatal("pendingRestart should be set while waiting for the old process")
	}
	if got := h.sp.count(); got != 1 {
		t.Fatalf("spawned %d children while the old one was still exiting, want 1", got)
	}
	if got := h.status("a"); got != StatusStarting {
		t.Fatalf("status = %q, want %q", got, StatusStarting)
	}

	// The old process finally exits: the queued start runs, and its exit must
	// not be mistaken for the new process's.
	first.exit(exitInfo{Code: 0})
	waitFor(t, 2*time.Second, "the queued start to spawn the replacement", func() bool {
		return h.sp.count() == 2
	})
	waitFor(t, 2*time.Second, "the replacement to be current", func() bool {
		var current bool
		h.withRec("a", func(r *rec) {
			current = r.child != nil && r.pendingRestart == false && r.lastExitCode == nil
		})
		return current
	})
	if got := h.status("a"); got == StatusStopped || got == StatusError {
		t.Fatalf("status = %q, want a starting/running replacement", got)
	}
}

func TestPendingRestartTimesOut(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.m.pendingWait = 50 * time.Millisecond

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.m.Stop("a")
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start while stopping: %v", err)
	}

	// The old process never reports an exit; the fallback timer must still get
	// the tunnel out of "starting".
	waitFor(t, 2*time.Second, "the pending wait to expire and relaunch", func() bool {
		return h.sp.count() == 2
	})
}

func TestStopDuringPendingRestartCancelsIt(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := h.sp.last()
	h.m.Stop("a")
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start while stopping: %v", err)
	}

	// The user presses stop again before the old process is gone.
	h.m.Stop("a")
	first.exit(exitInfo{Code: 0})

	time.Sleep(100 * time.Millisecond)
	if got := h.sp.count(); got != 1 {
		t.Fatalf("spawned %d children, want no relaunch after the second Stop", got)
	}
	if got := h.status("a"); got != StatusStopped {
		t.Fatalf("status = %q, want %q", got, StatusStopped)
	}
}

func TestSupersededExitIsIgnored(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := h.sp.last()

	h.m.Stop("a")
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start while stopping: %v", err)
	}
	first.exit(exitInfo{Code: 1})
	waitFor(t, 2*time.Second, "the replacement child", func() bool {
		return h.sp.count() == 2
	})

	// Give the (already ignored) old exit time to arrive out of order.
	time.Sleep(50 * time.Millisecond)

	var (
		status   string
		attempts int
		exitCode *int
		hasChild bool
	)
	h.withRec("a", func(r *rec) {
		status = r.status
		attempts = r.attempts
		exitCode = r.lastExitCode
		hasChild = r.child != nil
	})
	if status == StatusStopped || status == StatusError {
		t.Fatalf("status = %q: the old process's exit code clobbered the new record", status)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d: the old process's exit scheduled a restart", attempts)
	}
	if exitCode != nil {
		t.Fatalf("lastExitCode = %v: the old process's exit code was recorded", *exitCode)
	}
	if !hasChild {
		t.Fatal("the replacement child must stay attached to the record")
	}
}

func TestStopEscalatesToSIGKILL(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	child := h.sp.last()

	h.m.Stop("a")
	if !child.isTerminated() {
		t.Fatal("Stop must send SIGTERM first")
	}
	if got := h.status("a"); got != StatusStopping {
		t.Fatalf("status = %q, want %q", got, StatusStopping)
	}

	waitFor(t, 2*time.Second, "the SIGKILL fallback", child.isKilled)
	waitFor(t, 2*time.Second, "the process to be gone", func() bool {
		return h.status("a") == StatusStopped
	})
}

func TestStopClearsRestartTimerAndAttempts(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.m.baseDelay = 300 * time.Millisecond
	h.m.maxDelay = 300 * time.Millisecond

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.sp.last().exit(exitInfo{Code: 1})
	waitFor(t, 2*time.Second, "the restart timer", func() bool {
		var armed bool
		h.withRec("a", func(r *rec) { armed = r.restartTimer != nil })
		return armed
	})

	h.m.Stop("a")

	var armed bool
	h.withRec("a", func(r *rec) {
		armed = r.restartTimer != nil
		if r.attempts != 0 {
			t.Fatalf("attempts = %d, want 0 after Stop", r.attempts)
		}
	})
	if armed {
		t.Fatal("Stop must clear the restart timer")
	}

	time.Sleep(400 * time.Millisecond)
	if got := h.sp.count(); got != 1 {
		t.Fatalf("spawned %d children, want no restart after Stop", got)
	}
}

func TestStopUnknownIDLeavesNoRecord(t *testing.T) {
	h := newHarness(t)

	h.m.Stop("nope")
	h.m.StopAll()

	h.m.mu.Lock()
	records := len(h.m.recs)
	h.m.mu.Unlock()
	if records != 0 {
		t.Fatalf("%d runtime records left behind, want 0 (invariant 10)", records)
	}
}

func TestStopAllStopsEveryTunnel(t *testing.T) {
	h := newHarness(t, testTunnel("a"), testTunnel("b"))
	for _, id := range []string{"a", "b"} {
		if err := h.m.Start(id); err != nil {
			t.Fatalf("Start %s: %v", id, err)
		}
	}
	if h.sp.count() != 2 {
		t.Fatalf("spawned %d children, want 2", h.sp.count())
	}

	h.m.StopAll()

	for _, id := range []string{"a", "b"} {
		if got := h.status(id); got != StatusStopping {
			t.Fatalf("tunnel %s status = %q, want %q", id, got, StatusStopping)
		}
	}
	waitFor(t, 2*time.Second, "both children to be killed", func() bool {
		return h.sp.child(0).isKilled() && h.sp.child(1).isKilled()
	})
}

func TestWaitIdleReturnsOnceChildrenAreGone(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		h.m.WaitIdle(2 * time.Second)
		close(done)
	}()

	// Still alive: WaitIdle must block.
	select {
	case <-done:
		t.Fatal("WaitIdle returned while a child was still running")
	case <-time.After(50 * time.Millisecond):
	}

	h.sp.last().exit(exitInfo{Code: 0})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitIdle did not return after the child exited")
	}
}

func TestWaitIdleTimesOut(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	start := time.Now()
	h.m.WaitIdle(60 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("WaitIdle returned after %v, want it to wait out the timeout", elapsed)
	}
}

func TestRunningTracksLiveChild(t *testing.T) {
	h := newHarness(t, testTunnel("a"))

	if h.m.Running("a") {
		t.Fatal("a tunnel that never started must not report running")
	}
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !h.m.Running("a") {
		t.Fatal("Running must be true while the child is alive")
	}

	child := h.sp.last()
	child.exitOnTerminate = true
	h.m.Stop("a")
	waitFor(t, 2*time.Second, "the child to exit", func() bool {
		return !h.m.Running("a")
	})
	if h.m.Running("nope") {
		t.Fatal("Running must be false for an unknown tunnel")
	}
}

func TestChildOutputReachesRecordAndLogFile(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	child := h.sp.last()
	child.writeOutput("Registered tunnel connection connIndex=0\n")

	waitFor(t, 2*time.Second, "the child output to reach the record", func() bool {
		var got string
		h.withRec("a", func(r *rec) { got = r.url })
		return got != "" || h.status("a") == StatusRunning
	})

	if got := h.status("a"); got != StatusRunning {
		t.Fatalf("status = %q, want %q", got, StatusRunning)
	}

	lines := h.m.Logs("a", 10)
	if len(lines) != 1 {
		t.Fatalf("Logs returned %d lines, want 1: %v", len(lines), lines)
	}
	if !strings.HasSuffix(lines[0], "Registered tunnel connection connIndex=0") {
		t.Fatalf("log line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[0], "[") {
		t.Fatalf("log line must start with the UTC time prefix, got %q", lines[0])
	}
}

func TestSpawnFailureIsReported(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.sp.fail = errors.New("exec format error")

	err := h.m.Start("a")
	if err == nil || err.Error() != "启动失败: exec format error" {
		t.Fatalf("Start = %v, want the 启动失败 message", err)
	}
	var recorded string
	h.withRec("a", func(r *rec) { recorded = r.errMsg })
	if recorded != "启动失败: exec format error" {
		t.Fatalf("recorded error = %q", recorded)
	}
	if h.status("a") != StatusError {
		t.Fatalf("status = %q, want %q", h.status("a"), StatusError)
	}
}

func TestAutoRestartDisabledLeavesTunnelStopped(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	settings := h.store.Settings()
	settings.AutoRestart = false
	h.store.SetSettings(settings)

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.sp.last().exit(exitInfo{Code: 1})

	waitFor(t, 2*time.Second, "the exit to be recorded", func() bool {
		return h.status("a") == StatusError
	})
	var armed bool
	h.withRec("a", func(r *rec) { armed = r.restartTimer != nil })
	if armed {
		t.Fatal("autoRestart is off: no restart timer may be armed")
	}
}

func TestDisabledTunnelIsNotRestarted(t *testing.T) {
	tunnel := testTunnel("a")
	tunnel.Enabled = false
	h := newHarness(t, tunnel)

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.sp.last().exit(exitInfo{Code: 1})

	waitFor(t, 2*time.Second, "the exit to be recorded", func() bool {
		return h.status("a") == StatusError
	})
	var armed bool
	h.withRec("a", func(r *rec) { armed = r.restartTimer != nil })
	if armed {
		t.Fatal("a disabled tunnel must not be restarted")
	}
}

func TestStartUsesSettingsFromStore(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	settings := h.store.Settings()
	settings.Protocol = "quic"
	h.store.SetSettings(settings)

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.sp.count() != 1 {
		t.Fatalf("spawned %d children, want 1", h.sp.count())
	}
	seen, builds := h.cf.settingsSeen()
	if builds != 1 {
		t.Fatalf("BuildArgs called %d times, want 1", builds)
	}
	if seen.Protocol != "quic" {
		t.Fatalf("BuildArgs saw protocol %q, want the stored settings", seen.Protocol)
	}
}

// removeFile deletes a file, failing the test if it is still there.
func removeFile(name string) error {
	if err := os.Remove(name); err != nil {
		return err
	}
	if fileExists(name) {
		return errors.New("file still exists after removal: " + name)
	}
	return nil
}
