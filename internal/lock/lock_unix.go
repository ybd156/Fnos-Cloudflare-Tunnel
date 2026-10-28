//go:build linux || darwin || freebsd || netbsd || openbsd

package lock

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive advisory lock without blocking. The kernel releases
// it when the process exits for any reason, including SIGKILL, so a crashed
// instance never leaves the lock behind.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrAlreadyRunning
	}
	return err
}

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
