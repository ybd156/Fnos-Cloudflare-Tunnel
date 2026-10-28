// Package lock guarantees that at most one instance of the service supervises
// the tunnels.
//
// The lifecycle scripts already try to prevent duplicates by tracking a PID
// file, but a PID file is only a claim: two concurrent starts each rewrite it,
// and the loser's process keeps running while the file names the other one.
// That produced a real failure on a NAS -- two servers, two cloudflared
// children for the same tunnel, and a `status` command that reported "not
// running" the whole time, so every further start added another instance.
//
// An advisory file lock is held by the kernel for the lifetime of the process,
// so it cannot desynchronise from reality the way a written-down PID can.
package lock

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrAlreadyRunning reports that another process holds the lock.
var ErrAlreadyRunning = errors.New("另一个实例正在运行")

// Instance is a held single-instance lock.
type Instance struct {
	file    *os.File
	release func(*os.File)
}

// Acquire takes an exclusive, non-blocking lock on path, creating it if needed.
// It returns ErrAlreadyRunning when another process holds it.
func Acquire(path string) (*Instance, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// The file is never truncated: its contents are irrelevant, only the lock
	// state matters, and truncating would race with the other holder.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Instance{file: f, release: unlock}, nil
}

// Release drops the lock. It is safe to call more than once.
func (i *Instance) Release() {
	if i == nil || i.file == nil {
		return
	}
	i.release(i.file)
	_ = i.file.Close()
	i.file = nil
}
