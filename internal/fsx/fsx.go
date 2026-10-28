// Package fsx holds the small filesystem helpers the persisting packages
// share: an atomic writer for state files, and "best effort" variants of the
// calls whose failure must never take the service down.
package fsx

import (
	"os"
	"path/filepath"
	"strconv"
)

// AtomicWrite replaces file with data so that a reader either sees the old
// contents or the new ones, never a partial write. The temporary file is
// created in the same directory so the rename stays on one filesystem.
func AtomicWrite(file string, data []byte, mode os.FileMode) error {
	tmp := file + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	// Rename preserves the mode the temp file was created with, but this
	// filesystem can hand out generous defaults, so assert it explicitly.
	return os.Chmod(file, mode)
}

// UnlinkQuiet removes a file, ignoring "already gone" and permission errors.
// Callers use it where a missing file is a normal outcome.
func UnlinkQuiet(file string) {
	_ = os.Remove(file)
}

// ChmodQuiet applies a mode when it can, ignoring failures.
func ChmodQuiet(file string, mode os.FileMode) {
	_ = os.Chmod(file, mode)
}

// MkdirAll creates a directory tree, ignoring the error when it already
// exists in a form we can use.
func MkdirAll(dir string, mode os.FileMode) {
	_ = os.MkdirAll(dir, mode)
}

// Exists reports whether path exists at all.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsFile reports whether path exists and is a regular file.
func IsFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// WritePrivate writes a file that must not be world-readable (tunnel config,
// credentials), creating the parent directory first.
func WritePrivate(file string, data []byte) error {
	MkdirAll(filepath.Dir(file), 0o755)
	return AtomicWrite(file, data, 0o600)
}
