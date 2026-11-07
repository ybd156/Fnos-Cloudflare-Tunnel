package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

// An unusable directory that config-set does not need must not stop the App
// Center settings page from saving. EnsureDirs() reports the first failure
// among several unrelated paths (tmp, logs, credentials) precisely so that one
// bad path cannot take the others down; config-set used to treat that error as
// fatal, so a missing or read-only tmp directory made every wizard save fail
// while config.json stayed untouched.
func TestConfigSetWritesEvenWhenAnUnrelatedDirIsUnusable(t *testing.T) {
	root := t.TempDir()
	p := paths.Rooted(root)

	// A regular file where a directory is expected: creating anything below it
	// must fail, the way an unwritable volume does on the NAS.
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.PkgTmp = filepath.Join(blocker, "tmp")
	if err := p.EnsureDirs(); err == nil {
		t.Fatal("EnsureDirs should report the unusable tmp directory")
	}

	env := map[string]string{"wizard_log_level": "debug"}
	if err := configSet(p, func(key string) string { return env[key] }); err != nil {
		t.Fatalf("configSet = %v, want nil", err)
	}

	raw, err := os.ReadFile(p.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"logLevel": "debug"`) {
		t.Fatalf("config.json does not carry the wizard value: %s", raw)
	}
}

// The subcommand must still fail loudly when the one directory it does need
// cannot be created, instead of reporting a save that never happened.
func TestConfigSetReportsUnusableConfigDir(t *testing.T) {
	root := t.TempDir()
	p := paths.Rooted(root)

	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.ConfigFile = filepath.Join(blocker, "config.json")

	err := configSet(p, func(string) string { return "" })
	if err == nil {
		t.Fatal("configSet = nil, want an error naming the config directory")
	}
	if !strings.Contains(err.Error(), "无法创建数据目录") {
		t.Fatalf("error = %v", err)
	}
}
