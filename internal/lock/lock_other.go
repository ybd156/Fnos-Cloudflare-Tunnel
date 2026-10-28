//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package lock

import "os"

// The build hosts (Windows) have no flock(2) equivalent that Go exposes
// portably. The service only ever runs on the NAS, so on any other platform the
// lock is a no-op and the tests can still exercise the surrounding code.
func tryLock(*os.File) error { return nil }

func unlock(*os.File) {}
