package lintshell

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a checkout-shaped tree: cloudflared/cmd/*, build.sh and
// tools/test-lifecycle.sh.
func fixture(t *testing.T, scripts map[string]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range scripts {
		write(rel, body)
	}
	return root
}

const goodLib = `#!/bin/bash
APPNAME="${TRIM_APPNAME:-cloudflared}"
PKGVAR="${TRIM_PKGVAR:-/var/apps/${APPNAME}/var}"
fail() {
    printf '%s\n' "$1" >&2
    exit 1
}
`

const goodMain = `#!/bin/bash
set -u
. "$(dirname "$0")/_lib"
SERVER_BIN="${APPDEST}/bin/fncfd"
if [ ! -x "${SERVER_BIN}" ]; then
    fail "missing"
fi
exit 0
`

const goodUninstall = `#!/bin/bash
set -u
. "$(dirname "$0")/_lib"
case "${wizard_remove_data:-false}" in
true)
    rm -f "${PKGVAR}/server.log.1" "${PKGVAR}/main.log.1" 2>/dev/null || true
    rm -f "${PKGVAR}/VERSION" "${PKGVAR}/cloudflared.version" 2>/dev/null || true
    rm -f "${PKGVAR}"/*.log "${PKGVAR}"/*.log.1 2>/dev/null || true
    ;;
esac
exit 0
`

const goodConfig = `#!/bin/bash
set -u
. "$(dirname "$0")/_lib"
CONFIG_FILE="${PKGVAR}/config.json"
"${APPDEST}/bin/fncfd" config-set || fail "cannot write ${CONFIG_FILE}"
exit 0
`

const goodBuildSh = `#!/bin/bash
set -euo pipefail
GO_BIN="$(command -v go)"
exec "${GO_BIN}" run ./tools/build "$@"
`

const goodBuildPs1 = `$GoExe = Get-Command go
& $GoExe @('run', './tools/build')
`

const goodPipeline = `package main
// bash -n over every lifecycle script
var args = []string{"-n"}
`

const goodLifecycle = `#!/bin/bash
set -uo pipefail
go build -o "$out" ./cmd/fncfd
grep -Eq 'find_node' "$CMD"/*
`

// goodTree is the shape the checks are written for.
func goodTree() map[string]string {
	return map[string]string{
		"cloudflared/cmd/_lib":               goodLib,
		"cloudflared/cmd/main":               goodMain,
		"cloudflared/cmd/config_callback":    goodConfig,
		"cloudflared/cmd/uninstall_callback": goodUninstall,
		"cloudflared/cmd/install_init":       "#!/bin/bash\nset -u\nexit 0\n",
		"cloudflared/cmd/install_callback":   "#!/bin/bash\nset -u\nexit 0\n",
		"cloudflared/cmd/config_init":        "#!/bin/bash\nset -u\nexit 0\n",
		"cloudflared/cmd/uninstall_init":     "#!/bin/bash\nset -u\nexit 0\n",
		"cloudflared/cmd/upgrade_init":       "#!/bin/bash\nset -u\nexit 0\n",
		"cloudflared/cmd/upgrade_callback":   "#!/bin/bash\nset -u\nexit 0\n",
		"build.sh":                           goodBuildSh,
		"build.ps1":                          goodBuildPs1,
		"tools/test-lifecycle.sh":            goodLifecycle,
		"tools/build/main.go":                goodPipeline,
	}
}

func runLint(t *testing.T, root string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(root, &out)
	return out.String(), err
}

func TestRunPassesOnGoodTree(t *testing.T) {
	root := fixture(t, goodTree())
	out, err := runLint(t, root)
	if err != nil {
		t.Fatalf("unexpected failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ALL PASSED") {
		t.Fatalf("missing success line:\n%s", out)
	}
}

// The inversion that matters: a lifecycle script that invokes an interpreter
// used to be the rule and is now the defect.
func TestInterpreterInvocationFails(t *testing.T) {
	tree := goodTree()
	tree["cloudflared/cmd/main"] = goodMain + "\nnode -e 'console.log(1)'\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a node invocation in a lifecycle script was accepted\n%s", out)
	}
	if !strings.Contains(out, "cmd/* 不调用 node/python") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

// A comment mentioning node is not an invocation.
func TestInterpreterInCommentIsAllowed(t *testing.T) {
	tree := goodTree()
	tree["cloudflared/cmd/main"] = goodMain + "\n# this script used to need node and python\n"
	root := fixture(t, tree)
	if out, err := runLint(t, root); err != nil {
		t.Fatalf("a comment was treated as an invocation: %v\n%s", err, out)
	}
}

func TestBareTrimVariableFails(t *testing.T) {
	tree := goodTree()
	tree["cloudflared/cmd/main"] = "#!/bin/bash\nset -u\nmkdir -p \"${TRIM_PKGVAR}/logs\"\nexit 0\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a bare ${TRIM_PKGVAR} under set -u was accepted\n%s", out)
	}
	if !strings.Contains(out, "TRIM_PKGVAR") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestGuardedTrimVariableIsAllowed(t *testing.T) {
	tree := goodTree()
	tree["cloudflared/cmd/main"] = `#!/bin/bash
set -u
if [ -n "${TRIM_PKGVAR:-}" ]; then
    mkdir -p "${TRIM_PKGVAR}/logs"
fi
exit 0
`
	root := fixture(t, tree)
	if out, err := runLint(t, root); err != nil {
		t.Fatalf("a guarded variable was rejected: %v\n%s", err, out)
	}
}

func TestUnbalancedQuoteFails(t *testing.T) {
	tree := goodTree()
	tree["cloudflared/cmd/main"] = "#!/bin/bash\nset -u\necho \"unterminated\nexit 0\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("an unbalanced quote was accepted\n%s", out)
	}
	if !strings.Contains(out, "双引号成对") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestMissingLifecycleScriptFails(t *testing.T) {
	tree := goodTree()
	delete(tree, "cloudflared/cmd/upgrade_init")
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a missing lifecycle script was accepted\n%s", out)
	}
	if !strings.Contains(out, "upgrade_init 存在") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

// The wrappers must stay thin: a duplicated pipeline is how the two scripts
// drifted apart in the first place.
func TestFatBuildShFails(t *testing.T) {
	tree := goodTree()
	tree["build.sh"] = "#!/bin/bash\nset -euo pipefail\npython3 tools/make-icons.py\nexit 0\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a build.sh that implements the steps itself was accepted\n%s", out)
	}
	if !strings.Contains(out, "转调 tools/build") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestFatBuildPs1Fails(t *testing.T) {
	tree := goodTree()
	tree["build.ps1"] = "& $Py (Join-Path $Root 'tools\\normalize-fpk.py') $fpk\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a build.ps1 that inlines a pipeline step was accepted\n%s", out)
	}
	if !strings.Contains(out, "normalize-fpk") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestMissingBashSyntaxCheckFails(t *testing.T) {
	tree := goodTree()
	tree["tools/build/main.go"] = "package main\n// no syntax check here\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a pipeline without the bash -n step was accepted\n%s", out)
	}
	if !strings.Contains(out, "bash -n") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestUninstallLeftoversFail(t *testing.T) {
	tree := goodTree()
	tree["cloudflared/cmd/uninstall_callback"] = "#!/bin/bash\nset -u\nrm -f \"${PKGVAR}/config.json\"\nexit 0\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("an uninstall that leaves rotated logs behind was accepted\n%s", out)
	}
	if !strings.Contains(out, "server.log.1") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestConfigCallbackClobberingTunnelsFails(t *testing.T) {
	tree := goodTree()
	tree["cloudflared/cmd/config_callback"] = "#!/bin/bash\nset -u\ncfg.tunnels = []\nexit 0\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a config_callback that overwrites tunnels was accepted\n%s", out)
	}
	if !strings.Contains(out, "tunnels") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestLibProbingInterpreterFails(t *testing.T) {
	tree := goodTree()
	tree["cloudflared/cmd/_lib"] = goodLib + "\nfind_node() { command -v node; }\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a runtime probe in _lib was accepted\n%s", out)
	}
	if !strings.Contains(out, "find_node") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestLifecycleSuiteInterpreterFails(t *testing.T) {
	tree := goodTree()
	tree["tools/test-lifecycle.sh"] = "#!/bin/bash\nnode_eval() { node -e \"$1\"; }\n"
	root := fixture(t, tree)
	out, err := runLint(t, root)
	if err == nil {
		t.Fatalf("a lifecycle suite that calls node was accepted\n%s", out)
	}
	if !strings.Contains(out, "test-lifecycle.sh") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
