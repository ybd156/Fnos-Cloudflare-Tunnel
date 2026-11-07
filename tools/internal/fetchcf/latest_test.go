package fetchcf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ybd156/fnos-cloudflared/tools/internal/cfpin"
)

// writePin must rewrite the two literals and nothing else: the struct fields
// carry the same names, so a sloppy pattern would corrupt the type.
func TestWritePinRewritesOnlyTheLiterals(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tools", "internal", "cfpin", "cfpin.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	src := `package cfpin

type Pin struct {
	Version string
	Asset   string
	SHA256  string
}

var Pinned = Pin{
	Version: "2026.9.3",
	Asset:   "cloudflared-linux-amd64",
	SHA256:  "77e26d8d900e0b8469f416239d14b5f296525fdf79fee6f511ef55609e3fbac2",
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	next := cfpin.Pin{
		Version: "2026.10.7",
		Asset:   "cloudflared-linux-amd64",
		SHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := writePin(root, next); err != nil {
		t.Fatalf("writePin: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{`"2026.10.7"`, `"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("rewritten pin is missing %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "2026.9.3") {
		t.Fatalf("the old version survived:\n%s", text)
	}
	// The type declaration must be untouched.
	if !strings.Contains(text, "Version string") || !strings.Contains(text, "SHA256  string") {
		t.Fatalf("the struct declaration was damaged:\n%s", text)
	}
	// The asset name is not ours to change.
	if !strings.Contains(text, `Asset:   "cloudflared-linux-amd64"`) {
		t.Fatalf("the asset name was rewritten:\n%s", text)
	}
}

func TestWritePinFailsWithoutPinSource(t *testing.T) {
	if err := writePin(t.TempDir(), cfpin.Pin{Version: "x", SHA256: "y"}); err == nil {
		t.Fatal("writePin should fail when cfpin.go is absent")
	}
}

// An HTML error page saved as the asset would otherwise be hashed and pinned.
func TestVerifyELFRejectsNonELF(t *testing.T) {
	dir := t.TempDir()
	html := filepath.Join(dir, "page")
	if err := os.WriteFile(html, []byte("<!doctype html><h1>404</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyELF(html); err == nil {
		t.Fatal("verifyELF accepted an HTML body")
	}

	elf := filepath.Join(dir, "elf")
	if err := os.WriteFile(elf, []byte("\x7fELF\x02\x01\x01\x00rest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyELF(elf); err != nil {
		t.Fatalf("verifyELF rejected a real ELF header: %v", err)
	}

	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("\x7f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyELF(short); err == nil {
		t.Fatal("verifyELF accepted a truncated file")
	}
}
