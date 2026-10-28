// Package reporoot locates the repository root.
//
// Every tool here reads and writes paths relative to the checkout (the source
// tree under cloudflared/, the pinned binary in tools/, ...), so it needs one
// unambiguous answer to "where is the root?". Deriving it from go.mod means a
// tool works no matter which directory the caller ran it from -- `go run
// ./tools/lintui`, `cd tools && go run ./lintui`, or a wrapper script that
// changed directory first.
package reporoot

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrNotFound is returned when no go.mod exists at or above the start path.
var ErrNotFound = errors.New("repository root not found: no go.mod above the working directory")

// Find walks up from start until it finds the go.mod that marks the repository
// root, and returns that directory.
func Find(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		st, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil && !st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}

// FindFromCwd is Find applied to the process working directory.
func FindFromCwd() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return Find(wd)
}

// Resolve returns override when it is set, and the repository root otherwise.
// The tools expose a -root flag so tests (and a build from a copied tree) can
// point them at a scratch directory instead of the live checkout.
func Resolve(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	return FindFromCwd()
}
