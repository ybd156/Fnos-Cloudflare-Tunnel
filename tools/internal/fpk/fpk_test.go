package fpk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ybd156/fnos-cloudflared/tools/internal/cfpin"
)

// ---------------------------------------------------------------- fixtures

// entry is one member of a synthetic archive.
type entry struct {
	name string
	dir  bool
	data []byte
}

// windowsModes mimics fnpack on a Windows host: no executable bit, so every
// file lands 0666 and every directory 0777. That is exactly the defect
// Normalize exists to repair.
func windowsModes(_ string, isDir bool) int64 {
	if isDir {
		return 0o777
	}
	return 0o666
}

func buildGzTar(t *testing.T, entries []entry, mode func(string, bool) int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:    e.name,
			Mode:    mode(e.name, e.dir),
			ModTime: time.Unix(1700000000, 0),
			Format:  tar.FormatUSTAR,
		}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing header %q: %v", e.name, err)
		}
		if !e.dir {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatalf("writing body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// fixtureTree writes a miniature payload that satisfies every structural check.
func fixtureTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	files := map[string]string{
		"manifest": "appname = cloudflared\nversion = 1.0.0\ndesc = test build\n" +
			"os_min_version = 1.1.3100\nctl_stop = true\nchangelog = 1.0.0 first build\n",
		"ICON.PNG":               "png-64",
		"ICON_256.PNG":           "png-256",
		"config/privilege":       `{"defaults":{"run-as":"package"}}`,
		"config/resource":        `{"data-share":{}}`,
		"wizard/install":         `{"steps":[]}`,
		"wizard/config":          `{"steps":[]}`,
		"wizard/uninstall":       `{"steps":[]}`,
		"wizard/upgrade":         `{"steps":[]}`,
		"app/ui/config":          `{"entry":"cloudflared.main"}`,
		"app/www/index.html":     fixtureIndexHTML,
		"app/www/style.css":      fixtureCSS,
		"app/www/js/app.js":      "document.addEventListener('visibilitychange', () => {});\n",
		"cmd/main":               "#!/bin/bash\nset -u\nSERVER_BIN=\"${APPDEST}/bin/fncfd\"\nexec \"${SERVER_BIN}\"\nexit 0\n",
		"cmd/_lib":               "#!/bin/bash\nAPPNAME=\"${TRIM_APPNAME:-cloudflared}\"\n",
		"cmd/config_callback":    "#!/bin/bash\nset -u\n\"${APPDEST}/bin/fncfd\" config-set\nexit 0\n",
		"cmd/install_callback":   "#!/bin/bash\nset -u\nexit 0\n",
		"cmd/uninstall_callback": uninstallCallback,
		"cmd/upgrade_callback":   "#!/bin/bash\nset -u\nexit 0\n",
		"cmd/install_init":       "#!/bin/bash\nset -u\nexit 0\n",
		"cmd/config_init":        "#!/bin/bash\nset -u\nexit 0\n",
		"cmd/uninstall_init":     "#!/bin/bash\nset -u\nexit 0\n",
		"cmd/upgrade_init":       "#!/bin/bash\nset -u\nexit 0\n",
	}
	for rel, body := range files {
		writeFile(t, filepath.Join(src, filepath.FromSlash(rel)), []byte(body))
	}
	// The two ELF payloads. They are compared against size thresholds and magic
	// bytes, not against a source file, so filler content is enough.
	writeFile(t, filepath.Join(src, "app", "bin", "cloudflared"), fakeELF(512))
	writeFile(t, filepath.Join(src, "app", "bin", "fncfd"), fakeELF(512))
	return src
}

const fixtureIndexHTML = `<!doctype html>
<html><body>
<a class="skip-link" href="#main">skip</a>
<main id="view-tunnels"><div id="tunnel-summary"></div></main>
<div class="modal-backdrop hidden" id="modal-backdrop"></div>
<script type="module" src="%%CF_BASE%%/js/app.js"></script>
</body></html>
`

const fixtureCSS = `:root{--primary:#e05a0b}
@media (prefers-color-scheme: dark){:root{--primary:#f9a14a}}
@media (prefers-reduced-motion: reduce){*{animation:none}}
`

const uninstallCallback = `#!/bin/bash
set -u
rm -f "${PKGVAR}/server.log.1" "${PKGVAR}/VERSION" "${PKGVAR}/cloudflared.version"
exit 0
`

func fakeELF(size int) []byte {
	b := make([]byte, size)
	copy(b, "\x7fELF")
	return b
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// innerEntries collects everything that belongs inside app.tgz: the app/ tree
// plus the duplicate config/ copies fnpack adds.
func innerEntries(t *testing.T, src string) []entry {
	t.Helper()
	var out []entry
	appDir := filepath.Join(src, "app")
	err := filepath.WalkDir(appDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(appDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			out = append(out, entry{name: name + "/", dir: true})
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, entry{name: name, data: data})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"config/privilege", "config/resource"} {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry{name: rel, data: data})
	}
	return out
}

// packagedManifest mimics fnpack: it realigns the '=' padding and appends the
// checksum line. Byte identity therefore never holds for the manifest, which is
// why Verify compares it field by field.
func packagedManifest(t *testing.T, src string, appTgz []byte) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(src, "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), "version = ", "version     = ", 1)
	sum := md5.Sum(appTgz)
	return strings.TrimRight(text, "\n") + "\nchecksum = " + hex.EncodeToString(sum[:]) + "\n"
}

// buildFakeFPK assembles a .fpk the way fnpack would on a Windows host:
// correct structure, wrong permission bits.
func buildFakeFPK(t *testing.T, src string) []byte {
	t.Helper()
	appTgz := buildGzTar(t, innerEntries(t, src), windowsModes)

	outer := []entry{
		{name: "app.tgz", data: appTgz},
		{name: "manifest", data: []byte(packagedManifest(t, src, appTgz))},
	}
	for _, rel := range []string{"ICON.PNG", "ICON_256.PNG"} {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		outer = append(outer, entry{name: rel, data: data})
	}
	for _, dir := range []string{"cmd", "config", "wizard"} {
		outer = append(outer, entry{name: dir + "/", dir: true})
		entries, err := os.ReadDir(filepath.Join(src, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(src, dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			outer = append(outer, entry{name: dir + "/" + e.Name(), data: data})
		}
	}
	return buildGzTar(t, outer, windowsModes)
}

func testOptions() VerifyOptions {
	return VerifyOptions{
		MinBinaryBytes:   100,
		MinServerBytes:   100,
		MinAuthoredFiles: 5,
	}
}

// writeFPK writes raw bytes to a temp file and returns its path.
func writeFPK(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared.fpk")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// verifyToString runs Verify and returns its transcript plus the error.
func verifyToString(t *testing.T, fpkPath, src string, opt VerifyOptions) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Verify(fpkPath, src, opt, &out)
	return out.String(), err
}

func mustContain(t *testing.T, transcript, needle string) {
	t.Helper()
	if !strings.Contains(transcript, needle) {
		t.Fatalf("expected the verification to report %q\n--- transcript ---\n%s", needle, transcript)
	}
}

// ------------------------------------------------------------------- tests

func TestNormalizeThenVerifyPasses(t *testing.T) {
	src := fixtureTree(t)
	fpkPath := writeFPK(t, buildFakeFPK(t, src))

	// A Windows-hosted fnpack build is non-executable; the gate must say so.
	if transcript, err := verifyToString(t, fpkPath, src, testOptions()); err == nil {
		t.Fatalf("verify accepted an archive with 0666/0777 modes\n%s", transcript)
	} else {
		mustContain(t, transcript, "no unexpected 0666/0777 modes")
	}

	if _, err := Normalize(fpkPath, io.Discard); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if transcript, err := verifyToString(t, fpkPath, src, testOptions()); err != nil {
		t.Fatalf("verify failed after normalisation: %v\n%s", err, transcript)
	}
}

func TestNormalizeReportsChangedBitsAndChecksum(t *testing.T) {
	src := fixtureTree(t)
	fpkPath := writeFPK(t, buildFakeFPK(t, src))

	report, err := Normalize(fpkPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if report.OuterChanged == 0 || report.InnerChanged == 0 {
		t.Fatalf("expected both layers to change, got outer=%d inner=%d", report.OuterChanged, report.InnerChanged)
	}
	if len(report.Checksum) != 32 {
		t.Fatalf("checksum is not an md5 hex digest: %q", report.Checksum)
	}

	info, err := Summary(fpkPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.ManifestChecksum != report.Checksum {
		t.Fatalf("manifest checksum %q != reported %q", info.ManifestChecksum, report.Checksum)
	}
	if info.Size == 0 || len(info.SHA256) != 64 {
		t.Fatalf("unexpected summary: %+v", info)
	}
}

// A checksum that does not match app.tgz must be repaired by Normalize and
// rejected by Verify -- that field is what fnOS uses to accept the payload.
func TestChecksumIsRepairedAndEnforced(t *testing.T) {
	src := fixtureTree(t)
	fpkPath := writeFPK(t, buildFakeFPK(t, src))
	if _, err := Normalize(fpkPath, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Corrupt the checksum in the packaged manifest.
	raw, err := os.ReadFile(fpkPath)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := readTarGz(raw, "fpk")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range outer {
		if cleanName(m.name) == "manifest" {
			m.data = []byte(strings.Replace(string(m.data),
				"checksum = ", "checksum = 0000", 1))
		}
	}
	rebuilt, err := writeTarGz(outer, func(m *member) int64 {
		return wantMode(cleanName(m.name), m.isDir)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fpkPath, rebuilt, 0o644); err != nil {
		t.Fatal(err)
	}

	transcript, err := verifyToString(t, fpkPath, src, testOptions())
	if err == nil {
		t.Fatalf("verify accepted a wrong checksum\n%s", transcript)
	}
	mustContain(t, transcript, "checksum == md5(app.tgz)")

	if _, err := Normalize(fpkPath, io.Discard); err != nil {
		t.Fatal(err)
	}
	if transcript, err := verifyToString(t, fpkPath, src, testOptions()); err != nil {
		t.Fatalf("verify failed after the checksum repair: %v\n%s", err, transcript)
	}
}

// A stale artefact is the failure mode the gate exists for: one changed byte in
// the packaged tree must fail, with the file named.
func TestVerifyCatchesTamperedFile(t *testing.T) {
	src := fixtureTree(t)
	fpkPath := writeFPK(t, buildFakeFPK(t, src))
	if _, err := Normalize(fpkPath, io.Discard); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(fpkPath)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := readTarGz(raw, "fpk")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range outer {
		if cleanName(m.name) == "cmd/main" {
			m.data = append(m.data, []byte("# stale\n")...)
		}
	}
	rebuilt, err := writeTarGz(outer, func(m *member) int64 {
		return wantMode(cleanName(m.name), m.isDir)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fpkPath, rebuilt, 0o644); err != nil {
		t.Fatal(err)
	}

	transcript, err := verifyToString(t, fpkPath, src, testOptions())
	if err == nil {
		t.Fatalf("verify accepted a tampered cmd/main\n%s", transcript)
	}
	mustContain(t, transcript, "cmd/main matches source")
}

func TestVerifyCatchesMissingMember(t *testing.T) {
	src := fixtureTree(t)
	fpkPath := writeFPK(t, buildFakeFPK(t, src))
	if _, err := Normalize(fpkPath, io.Discard); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(fpkPath)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := readTarGz(raw, "fpk")
	if err != nil {
		t.Fatal(err)
	}
	kept := outer[:0]
	for _, m := range outer {
		if cleanName(m.name) == "wizard/install" {
			continue
		}
		kept = append(kept, m)
	}
	rebuilt, err := writeTarGz(kept, func(m *member) int64 {
		return wantMode(cleanName(m.name), m.isDir)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fpkPath, rebuilt, 0o644); err != nil {
		t.Fatal(err)
	}

	transcript, err := verifyToString(t, fpkPath, src, testOptions())
	if err == nil {
		t.Fatalf("verify accepted an archive without wizard/install\n%s", transcript)
	}
	mustContain(t, transcript, "outer contains wizard/install")
	mustContain(t, transcript, "packaged file missing: wizard/install")
}

// The whole point of the refactor: a lifecycle script that reaches for an
// interpreter must not be packagable.
func TestVerifyCatchesInterpreterInScripts(t *testing.T) {
	src := fixtureTree(t)
	writeFile(t, filepath.Join(src, "cmd", "install_init"),
		[]byte("#!/bin/bash\nset -u\nnode -e 'console.log(1)'\nexit 0\n"))

	fpkPath := writeFPK(t, buildFakeFPK(t, src))
	if _, err := Normalize(fpkPath, io.Discard); err != nil {
		t.Fatal(err)
	}
	transcript, err := verifyToString(t, fpkPath, src, testOptions())
	if err == nil {
		t.Fatalf("verify accepted a script that invokes node\n%s", transcript)
	}
	mustContain(t, transcript, "packaged cmd/* scripts do not invoke node/python")
}

func TestVerifyCatchesNodeDependency(t *testing.T) {
	src := fixtureTree(t)
	manifest := "appname = cloudflared\nversion = 1.0.0\ndesc = test build\n" +
		"os_min_version = 1.1.3100\ninstall_dep_apps = nodejs_v24\nchangelog = 1.0.0 first build\n"
	writeFile(t, filepath.Join(src, "manifest"), []byte(manifest))

	fpkPath := writeFPK(t, buildFakeFPK(t, src))
	if _, err := Normalize(fpkPath, io.Discard); err != nil {
		t.Fatal(err)
	}
	transcript, err := verifyToString(t, fpkPath, src, testOptions())
	if err == nil {
		t.Fatalf("verify accepted a manifest that declares a Node dependency\n%s", transcript)
	}
	mustContain(t, transcript, "manifest declares no Node runtime dependency")
}

func TestVerifyCatchesWrongPin(t *testing.T) {
	src := fixtureTree(t)
	fpkPath := writeFPK(t, buildFakeFPK(t, src))
	if _, err := Normalize(fpkPath, io.Discard); err != nil {
		t.Fatal(err)
	}

	opt := testOptions()
	opt.CheckPin = true
	opt.Pin = cfpin.Pin{
		Version: "2026.9.3",
		Asset:   "cloudflared-linux-amd64",
		SHA256:  strings.Repeat("ab", 32),
	}

	transcript, err := verifyToString(t, fpkPath, src, opt)
	if err == nil {
		t.Fatalf("verify accepted a binary that does not match the pin\n%s", transcript)
	}
	mustContain(t, transcript, "the pinned version string really occurs in the packaged binary")
	mustContain(t, transcript, "packaged binary matches the pinned SHA-256")
}

// Normalize must refuse to rebuild an archive it cannot represent faithfully,
// rather than silently turning a symlink into an empty file.
func TestNormalizeRejectsUnsupportedMember(t *testing.T) {
	src := fixtureTree(t)
	raw := buildFakeFPK(t, src)

	outer, err := readTarGz(raw, "fpk")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range outer {
		hdr := m.hdr
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if !m.isDir {
			if _, err := tw.Write(m.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "link", Typeflag: tar.TypeSymlink, Linkname: "app.tgz", Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	fpkPath := writeFPK(t, gz.Bytes())
	if _, err := Normalize(fpkPath, io.Discard); err == nil {
		t.Fatal("normalize accepted a symlink member")
	}
}
