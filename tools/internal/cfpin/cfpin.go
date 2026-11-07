// Package cfpin is the single source of truth for which upstream cloudflared
// binary the package bundles.
//
// The 38MB binary is deliberately not tracked in git (see .gitignore): it is a
// third-party build that upstream already publishes, so committing it would put
// it into every clone and into the permanent history for a file nobody in this
// repository edits. The pinned release is fetched instead, and its SHA-256 is
// verified before the bytes are put where the build picks them up -- so a
// truncated download, a hijacked mirror, or a silently republished upstream tag
// cannot end up inside a package.
//
// This used to live in tools/fetch-cloudflared.py, where tools/verify-fpk.py
// re-read it with a regular expression so the two could not drift. As a Go
// package the compiler enforces the same single-owner property, and the
// artefact check still compares the packaged bytes against SHA-256 and the
// version string below.
package cfpin

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// Pin describes one upstream release. It is a struct rather than three
// constants so tests can exercise the verification paths against a fake pin
// instead of downloading 38MB.
type Pin struct {
	Version string
	Asset   string
	SHA256  string
}

// URL is the release asset this pin refers to.
func (p Pin) URL() string {
	return "https://github.com/cloudflare/cloudflared/releases/download/" + p.Version + "/" + p.Asset
}

// Pinned is the release the .fpk bundles. It must stay in step with the version
// the package actually ships: tools/fpk verify asserts both that the packaged
// binary hashes to SHA256 and that Version really occurs inside it, so a
// version bumped without re-deriving the hash (or a swapped same-version
// rebuild) fails the build.
var Pinned = Pin{
	Version: "2026.10.7",
	Asset:   "cloudflared-linux-amd64",
	SHA256:  "1f99fb518b83dec2ba8d870a7cc991d82a77e6196295a3d2754ba17ab9f91872",
}

// Dest is where the binary must live for fnpack to pick it up.
func Dest(root string) string {
	return filepath.Join(root, "cloudflared", "app", "bin", "cloudflared")
}

// HashFile returns the hex SHA-256 of a file, streaming so the 38MB binary does
// not have to be held in memory.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
