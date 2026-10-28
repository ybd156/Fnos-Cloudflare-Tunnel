//go:build linux || darwin || freebsd || netbsd || openbsd

package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSecondAcquireIsRefused is the guarantee the NAS needs: a second service
// instance must not be able to supervise the same tunnels. A stale PID file let
// exactly that happen, and two cloudflared processes for one tunnel fight over
// the connection.
func TestSecondAcquireIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("首次获取锁失败: %v", err)
	}
	defer first.Release()

	second, err := Acquire(path)
	if !errors.Is(err, ErrAlreadyRunning) {
		if second != nil {
			second.Release()
		}
		t.Fatalf("第二次获取应返回 ErrAlreadyRunning，得到 %v", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("首次获取锁失败: %v", err)
	}
	first.Release()

	// Releasing twice must be safe: shutdown paths are not always single-shot.
	first.Release()

	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("释放后应能重新获取，得到 %v", err)
	}
	second.Release()
}

// TestLockSurvivesStaleFile covers the exact NAS situation: the lock file exists
// on disk but no process holds it. A leftover file must not block a restart.
func TestLockSurvivesStaleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.lock")
	if err := os.WriteFile(path, []byte("3029965\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inst, err := Acquire(path)
	if err != nil {
		t.Fatalf("残留的锁文件不应阻止启动: %v", err)
	}
	inst.Release()
}
