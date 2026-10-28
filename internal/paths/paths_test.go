package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootedLayout(t *testing.T) {
	root := t.TempDir()
	p := Rooted(root)

	// Everything must live under root: a test tree must never touch /var/apps.
	for name, path := range map[string]string{
		"AppDest": p.AppDest, "PkgVar": p.PkgVar, "PkgTmp": p.PkgTmp,
		"WWW": p.WWW, "CFBin": p.CFBin, "ServerBin": p.ServerBin,
		"SocketPath": p.SocketPath, "CFDir": p.CFDir, "CertFile": p.CertFile,
		"TunnelsDir": p.TunnelsDir, "LogsDir": p.LogsDir,
		"ConfigFile": p.ConfigFile, "AppLog": p.AppLog, "PIDFile": p.PIDFile,
	} {
		within, err := filepath.Rel(root, path)
		if err != nil || strings.HasPrefix(within, "..") || filepath.IsAbs(within) {
			t.Errorf("%s 不在测试根目录下: %s", name, path)
		}
	}

	if p.GatewayPrefix != "/app/"+p.AppName {
		t.Errorf("网关前缀应由 appname 推出，得到 %q", p.GatewayPrefix)
	}
	if got := p.TunnelConfig("t_1"); got != filepath.Join(p.TunnelsDir, "t_1", "config.yml") {
		t.Errorf("TunnelConfig 路径不符: %s", got)
	}
	if got := p.TunnelLog("t_1"); got != filepath.Join(p.LogsDir, "t_1.log") {
		t.Errorf("TunnelLog 路径不符: %s", got)
	}
	if got := p.CredentialFile("uuid"); got != filepath.Join(p.CFDir, "uuid.json") {
		t.Errorf("CredentialFile 路径不符: %s", got)
	}
}

func TestEnsureDirsCreatesEverything(t *testing.T) {
	p := Rooted(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs 失败: %v", err)
	}
	for _, dir := range []string{p.PkgVar, p.PkgTmp, p.CFDir, p.TunnelsDir, p.LogsDir} {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			t.Errorf("目录未创建: %s", dir)
		}
	}
}

// TestEnsureDirsIsNotBlockedByOneBadDirectory pins the behaviour that made a
// defaulted TRIM_PKGTMP take the whole service down: a single unusable path must
// not prevent the data directory from being created.
func TestEnsureDirsIsNotBlockedByOneBadDirectory(t *testing.T) {
	p := Rooted(t.TempDir())
	// A regular file where a directory is expected: MkdirAll fails with ENOTDIR.
	if err := os.MkdirAll(filepath.Dir(p.PkgTmp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PkgTmp, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := p.EnsureDirs()
	if err == nil {
		t.Error("应报告无法创建的目录")
	}
	// The directories that could be created must still exist.
	for _, dir := range []string{p.PkgVar, p.CFDir, p.TunnelsDir, p.LogsDir} {
		if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
			t.Errorf("一个目录失败不应阻止其它目录创建: %s", dir)
		}
	}
}
