// Package fpk reads, rewrites and verifies the two-layer fnOS .fpk archive.
//
// A .fpk is a gzip'd tar whose members are `app.tgz` (itself a gzip'd tar with
// the whole app/ tree plus a duplicate config/), `manifest` (with an appended
// `checksum = <md5 of app.tgz>`), `cmd/*`, `wizard/*`, `config/*` and the two
// icons -- see FPK-SPEC.md §1.3.
//
// Two things in this package exist because they caught real defects:
//
//   - Normalize rewrites the permission bits in both tar layers. fnpack takes
//     archive modes from the host filesystem's stat(); a Windows host has no
//     executable bit, so every file lands as 0666 and every directory as 0777,
//     and cmd/* plus bin/cloudflared reach the NAS non-executable -- the app
//     then cannot start at all.
//   - Verify re-opens the artefact and compares it to the source tree.
//     `fnpack build` prints "Packing successfully" as long as the required
//     files exist, so a stale tree ships green without this gate.
package fpk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

// Permissions the archive must carry. These are the Linux-correct values the
// installed tree needs; see wantMode.
const (
	dirMode  = 0o755
	fileMode = 0o644
	execMode = 0o755
)

// execNames are the payload files that must be runnable on the target, keyed by
// their path inside the tar that holds them. bin/cloudflared is the upstream
// tunnel binary the service spawns; bin/fncfd is the service itself.
var execNames = map[string]bool{
	"bin/cloudflared": true,
	"bin/fncfd":       true,
}

// wantMode is the single definition of "correct mode" used by both Normalize
// and Verify -- if they disagreed, a build could normalise to one set of bits
// and then fail its own verification.
func wantMode(name string, isDir bool) int64 {
	if isDir {
		return dirMode
	}
	if execNames[name] {
		return execMode
	}
	if strings.HasPrefix(name, "cmd/") {
		return execMode
	}
	return fileMode
}

// member is one tar entry plus its body. The original header is kept so that
// Normalize changes the mode and nothing else.
type member struct {
	name  string
	isDir bool
	hdr   tar.Header
	data  []byte
}

// cleanName drops the trailing slash a directory entry carries, so presence
// checks work whether the packer wrote "cmd" or "cmd/".
func cleanName(name string) string {
	return strings.TrimSuffix(name, "/")
}

// readTarGz parses a gzip'd tar held in memory.
//
// Only regular files and directories are accepted. A symlink, hardlink or
// device would have to be re-emitted as something else -- silently rewriting it
// as an empty regular file corrupts the archive -- so it is rejected instead.
func readTarGz(raw []byte, label string) ([]*member, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: not a gzip stream: %w", label, err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	var out []*member
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
		default:
			return nil, fmt.Errorf("%s: unsupported tar member type %q for %q; this tool only handles regular files and directories",
				label, string(hdr.Typeflag), hdr.Name)
		}
		m := &member{name: hdr.Name, isDir: hdr.Typeflag == tar.TypeDir, hdr: *hdr}
		if !m.isDir {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("%s: reading %q: %w", label, hdr.Name, err)
			}
			m.data = data
		}
		out = append(out, m)
	}
	return out, nil
}

// writeTarGz rebuilds a gzip'd tar, preserving every header field except the
// mode (and the size, which follows the body).
//
// The gzip header carries no mtime so two normalisations of the same input
// produce the same bytes -- the manifest checksum is the md5 of this stream, so
// a stray timestamp would make the artefact irreproducible for no reason.
func writeTarGz(members []*member, modeOf func(*member) int64) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range members {
		hdr := m.hdr
		hdr.Mode = modeOf(m)
		hdr.PAXRecords = nil
		hdr.Format = tar.FormatUSTAR
		if m.isDir {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(m.data))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			// A name too long for USTAR (or another USTAR limit) must not
			// truncate the entry: re-emit it as PAX, which can carry it.
			hdr.Format = tar.FormatPAX
			if err2 := tw.WriteHeader(&hdr); err2 != nil {
				return nil, fmt.Errorf("writing %q: %w", m.name, err2)
			}
		}
		if !m.isDir {
			if _, err := tw.Write(m.data); err != nil {
				return nil, fmt.Errorf("writing %q: %w", m.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}

	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	zw.Header.ModTime = time.Time{}
	if _, err := zw.Write(buf.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return gz.Bytes(), nil
}

// md5hex is the digest the packaged manifest records for app.tgz.
func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// index maps cleaned member names to members. The last entry wins, which is
// what a tar reader would do as well.
func index(members []*member) map[string]*member {
	m := make(map[string]*member, len(members))
	for _, mem := range members {
		m[cleanName(mem.name)] = mem
	}
	return m
}

// ModeReport describes what Normalize changed.
type ModeReport struct {
	OuterChanged int
	InnerChanged int
	Checksum     string
	Size         int64
}

// Info summarises a built artefact for the build's closing report.
type Info struct {
	Size             int64
	SHA256           string
	ManifestChecksum string
}

// TarMembers returns the member names of a gzip'd tar stream, for diagnostics.
func TarMembers(raw []byte, label string) ([]string, error) {
	members, err := readTarGz(raw, label)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.name)
	}
	return names, nil
}

// joinSlash builds an archive path from a source-tree relative path.
func joinSlash(parts ...string) string {
	return path.Join(parts...)
}

// fileExists is a tiny helper kept here so normalize and verify agree on what
// "present" means (a directory named like a file is not present).
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
