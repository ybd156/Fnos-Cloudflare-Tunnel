package fetchcf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ybd156/fnos-cloudflared/tools/internal/cfpin"
)

// installFake puts bytes where the pin expects the binary and returns a pin
// whose SHA-256 matches them, so the local verification paths can be exercised
// without downloading 38MB.
func installFake(t *testing.T, body []byte) (string, cfpin.Pin) {
	t.Helper()
	root := t.TempDir()
	dest := cfpin.Dest(root)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, body, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return root, cfpin.Pin{
		Version: "2026.9.3",
		Asset:   "cloudflared-linux-amd64",
		SHA256:  hex.EncodeToString(sum[:]),
	}
}

func TestPresentAndMatchingIsReused(t *testing.T) {
	root, pin := installFake(t, []byte("fake cloudflared"))
	var out, errOut bytes.Buffer

	if err := RunWithPin(root, pin, Options{}, &out, &errOut); err != nil {
		t.Fatalf("a correct local copy was not reused: %v (%s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "already present and matches the pin") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	// It must not have touched the file.
	body, err := os.ReadFile(cfpin.Dest(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "fake cloudflared" {
		t.Fatalf("the present binary was rewritten: %q", body)
	}
}

func TestPresentWithWrongHashFailsUnderCheck(t *testing.T) {
	root, pin := installFake(t, []byte("tampered"))
	pin.SHA256 = strings.Repeat("00", 32)
	var out, errOut bytes.Buffer

	err := RunWithPin(root, pin, Options{Check: true}, &out, &errOut)
	if err == nil {
		t.Fatal("a binary that does not match the pin was accepted")
	}
	if !strings.Contains(out.String(), "present but the hash differs") {
		t.Fatalf("the mismatch was not reported:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "--check: not downloading") {
		t.Fatalf("--check did not refuse to download:\n%s", errOut.String())
	}
}

func TestMissingBinaryFailsUnderCheck(t *testing.T) {
	root := t.TempDir()
	pin := cfpin.Pin{Version: "2026.9.3", Asset: "cloudflared-linux-amd64", SHA256: strings.Repeat("ab", 32)}
	var out, errOut bytes.Buffer

	if err := RunWithPin(root, pin, Options{Check: true}, &out, &errOut); err == nil {
		t.Fatal("a missing binary was accepted by --check")
	}
	if !strings.Contains(errOut.String(), "is missing") {
		t.Fatalf("unexpected stderr:\n%s", errOut.String())
	}
}

// The failure path must say how to recover by hand, because the build cannot
// proceed and the NAS may be behind a mirror.
func TestDownloadFailureExplainsRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the network-failure probe in -short mode")
	}
	root := t.TempDir()
	// A version that cannot exist makes the release URL 404 without needing a
	// working connection to the asset host.
	pin := cfpin.Pin{Version: "0.0.0-does-not-exist", Asset: "cloudflared-linux-amd64", SHA256: strings.Repeat("ab", 32)}
	var out, errOut bytes.Buffer

	if err := RunWithPin(root, pin, Options{}, &out, &errOut); err == nil {
		t.Fatal("a download of a non-existent release succeeded")
	}
	combined := errOut.String()
	if !strings.Contains(combined, "fetch it by hand") {
		t.Fatalf("the failure does not explain how to recover:\n%s", combined)
	}
	if !strings.Contains(combined, "expected sha256") {
		t.Fatalf("the failure does not state the expected hash:\n%s", combined)
	}
	// Nothing must be left behind where the build would pick it up.
	if _, err := os.Stat(cfpin.Dest(root)); err == nil {
		t.Fatal("a failed download left a file at the destination")
	}
	if _, err := os.Stat(cfpin.Dest(root) + ".part"); err == nil {
		t.Fatal("a failed download left a .part file behind")
	}
}
