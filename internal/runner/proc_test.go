package runner

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary act as the child process for the spawner tests.
// Mode 1 behaves like a cloudflared that prints and fails; mode 2 behaves like
// one that stays up until it is told to stop.
func TestMain(m *testing.M) {
	switch os.Getenv("RUNNER_TEST_HELPER") {
	case "1":
		fmt.Fprintln(os.Stdout, "Registered tunnel connection connIndex=0")
		fmt.Fprintln(os.Stderr, "ERR helper wrote to stderr")
		os.Exit(3)
	case "2":
		fmt.Fprintln(os.Stdout, "INF helper staying up")
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperEnv(mode string) []string {
	return append(os.Environ(), "RUNNER_TEST_HELPER="+mode)
}

func selfExecutable(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return self
}

func TestSpawnExecCapturesOutputAndExitCode(t *testing.T) {
	child, err := spawnExec(selfExecutable(t), nil, helperEnv("1"), t.TempDir())
	if err != nil {
		t.Fatalf("spawnExec: %v", err)
	}
	if child.PID() <= 0 {
		t.Fatalf("PID = %d, want a real process id", child.PID())
	}

	// Both pipes must reach EOF on their own once the process exits: that is
	// what makes the supervisor's readers terminate.
	stdout, err := io.ReadAll(child.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	stderr, err := io.ReadAll(child.Stderr())
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}

	info := child.Wait()
	if info.Signaled {
		t.Fatal("the helper exits normally, not by signal")
	}
	if info.Code != 3 {
		t.Fatalf("exit code = %d, want 3", info.Code)
	}
	if !strings.Contains(string(stdout), "Registered tunnel connection") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(string(stderr), "ERR helper wrote to stderr") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestSpawnExecReportsMissingBinary(t *testing.T) {
	child, err := spawnExec("/definitely/not/here/cloudflared", nil, helperEnv("1"), t.TempDir())
	if err == nil {
		if child != nil {
			_ = child.Kill()
		}
		t.Fatal("spawning a missing binary must fail")
	}
}

// TestStopEndsARealChild exercises the escalation against a real process:
// SIGTERM ends it where signals exist, and the SIGKILL fallback covers the
// platforms where they do not.
func TestStopEndsARealChild(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	self := selfExecutable(t)
	h.m.spawn = func(bin string, args, env []string, dir string) (childProcess, error) {
		return spawnExec(self, nil, helperEnv("2"), h.p.PkgVar)
	}
	h.m.killGrace = 200 * time.Millisecond

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !h.m.Running("a") {
		t.Fatal("the real child should be running")
	}

	h.m.Stop("a")
	waitFor(t, 10*time.Second, "the real child to be gone", func() bool {
		return !h.m.Running("a")
	})
	if got := h.status("a"); got != StatusStopped {
		t.Fatalf("status = %q, want %q", got, StatusStopped)
	}

	var (
		errMsg string
		code   *int
	)
	h.withRec("a", func(r *rec) {
		errMsg = r.errMsg
		code = r.lastExitCode
	})
	if errMsg != "" {
		t.Fatalf("error = %q, want empty after a clean stop", errMsg)
	}
	// A process ended by a signal has no exit code, which is why the previous
	// implementation reported null.
	if code != nil {
		t.Fatalf("lastExitCode = %d, want null for a signalled process", *code)
	}
}

// TestRealChildExitIsRecorded runs a real child to completion through the
// supervisor and checks the record it leaves behind.
func TestRealChildExitIsRecorded(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	settings := h.store.Settings()
	settings.AutoRestart = false
	h.store.SetSettings(settings)

	self := selfExecutable(t)
	h.m.spawn = func(bin string, args, env []string, dir string) (childProcess, error) {
		return spawnExec(self, nil, helperEnv("1"), h.p.PkgVar)
	}

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 10*time.Second, "the real child to exit", func() bool {
		return h.status("a") == StatusError
	})

	var code *int
	h.withRec("a", func(r *rec) { code = r.lastExitCode })
	if code == nil || *code != 3 {
		t.Fatalf("lastExitCode = %v, want 3", code)
	}

	// The child's stdout must have reached the record before the exit was
	// handled, or the UI would show an empty log for a failed start.
	waitFor(t, 5*time.Second, "the child output to be logged", func() bool {
		lines := h.m.Logs("a", 10)
		for _, line := range lines {
			if strings.Contains(line, "Registered tunnel connection") {
				return true
			}
		}
		return false
	})
}
