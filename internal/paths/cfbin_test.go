package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// TestActiveCFBinPrefersUpdated covers the reported defect: a user updated
// cloudflared from the UI, then installed a new app version and found the old
// bundled binary back. The updated binary lives in $PKGVAR precisely because the
// payload is replaced on every upgrade.
func TestActiveCFBinPrefersUpdated(t *testing.T) {
	p := Rooted(t.TempDir())

	// Fresh install: nothing updated yet, so the bundled copy is used.
	if got := p.ActiveCFBin(); got != p.CFBin {
		t.Errorf("没有更新过的二进制时应使用内置版本，得到 %s", got)
	}

	// After an in-place update the var copy wins.
	if err := os.MkdirAll(filepath.Dir(p.CFBinUpdated), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.CFBinUpdated, []byte("updated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := p.ActiveCFBin(); got != p.CFBinUpdated {
		t.Errorf("存在更新版本时应优先使用它，得到 %s", got)
	}

	// The two paths must live on different sides of the payload boundary: that
	// is the whole point. If the updated copy ever moved into AppDest, the next
	// package install would silently revert it again.
	within, err := filepath.Rel(p.AppDest, p.CFBinUpdated)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(within) && within[0] != '.' && within[:2] != ".." {
		t.Errorf("已更新的二进制不能放在载荷目录内（会被升级覆盖），实际是 %s", p.CFBinUpdated)
	}
}

func TestActiveCFBinIgnoresDirectory(t *testing.T) {
	p := Rooted(t.TempDir())
	// A directory at the updated path must not be mistaken for a binary: running
	// it would fail with a confusing error instead of falling back.
	if err := os.MkdirAll(p.CFBinUpdated, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := p.ActiveCFBin(); got != p.CFBin {
		t.Errorf("目录不是可执行文件，应回落到内置版本，得到 %s", got)
	}
}

// TestRootedKeepsUpdatedBinaryUnderPkgVar pins the layout the uninstall script
// and the installer both depend on.
func TestRootedKeepsUpdatedBinaryUnderPkgVar(t *testing.T) {
	p := Rooted(t.TempDir())
	want := filepath.Join(p.PkgVar, "bin", "cloudflared")
	if p.CFBinUpdated != want {
		t.Errorf("CFBinUpdated = %s, want %s", p.CFBinUpdated, want)
	}
}
