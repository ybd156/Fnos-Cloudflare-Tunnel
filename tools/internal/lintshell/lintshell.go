// Package lintshell runs the static checks for the fnOS lifecycle scripts and
// the build wrappers.
//
// Why this exists: the lifecycle scripts run on the NAS, where no developer can
// iterate quickly -- and on a Windows host there is no way to run them at all.
// A shell edit therefore used to ship on inspection alone, and the mistakes a
// shell edit actually makes are all invisible to the eye:
//
//   - an unbound variable under `set -u` (the script dies in the App Center's
//     environment but works in the author's shell, where the variable happens
//     to be exported);
//   - an unbalanced quote, which silently swallows the rest of the file;
//   - a bare `node`/`python` on a host where neither is on PATH.
//
// That last one is now inverted. The package no longer depends on Node at all:
// the service is a static Go binary and the scripts exec it directly. So the
// check asserts the scripts do *not* reach for an interpreter -- if one ever
// comes back, the "no Node dependency" claim in the manifest and the README is
// false again.
//
// These are textual checks, not a substitute for running the suite on the NAS.
// They catch the class of error that inspection misses, and they are cheap.
package lintshell

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ybd156/fnos-cloudflared/tools/internal/shellscan"
)

// lifecycleScripts are the entry points the app centre invokes. cmd/_lib is not
// in the list: it is a sourced helper, and a tree without one is legitimate as
// long as the entry points are there.
var lifecycleScripts = []string{
	"main",
	"install_init", "install_callback",
	"config_init", "config_callback",
	"uninstall_init", "uninstall_callback",
	"upgrade_init", "upgrade_callback",
}

var (
	guardRE     = regexp.MustCompile(`\[\s+-n\s+"\$\{(TRIM_[A-Z_]+):-\}"\s*\]`)
	bareTrimRE  = regexp.MustCompile(`\$\{(TRIM_[A-Z_]+)\}`)
	blockEndRE  = regexp.MustCompile(`^\s*(fi|esac|done)\b`)
	pyToolRE    = regexp.MustCompile(`tools/[A-Za-z0-9_-]+\.(py|mjs)`)
	nodeToolRE  = regexp.MustCompile(`\b(node|npm|pnpm|python3?)\b`)
	truncateRE  = regexp.MustCompile(`(?m)\bcat\s*>+\s*"?[^\n]*config\.json`)
	tunnelsAsgn = regexp.MustCompile(`\.tunnels\s*=[^=]`)
)

type linter struct {
	out      io.Writer
	root     string
	passed   int
	failures []string
}

// ck records one assertion. detail is optional and is appended to the label
// when the check fails, so a failure says what was actually seen.
func (l *linter) ck(cond bool, label string, detail ...string) {
	if cond {
		l.passed++
		fmt.Fprintf(l.out, "  ok   %s\n", label)
		return
	}
	msg := label
	if len(detail) > 0 && detail[0] != "" {
		msg += " (" + detail[0] + ")"
	}
	l.failures = append(l.failures, msg)
	fmt.Fprintf(l.out, "  FAIL %s\n", msg)
}

func (l *linter) section(title string) {
	fmt.Fprintf(l.out, "\n%s\n", title)
}

func (l *linter) read(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(l.root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (l *linter) exists(rel string) bool {
	st, err := os.Stat(filepath.Join(l.root, filepath.FromSlash(rel)))
	return err == nil && !st.IsDir()
}

// Run lints the checkout at root.
func Run(root string, out io.Writer) error {
	l := &linter{out: out, root: root}

	cmdDir := filepath.Join(root, "cloudflared", "cmd")
	cmdFiles, err := listFiles(cmdDir)
	if err != nil {
		return fmt.Errorf("cannot list cloudflared/cmd: %w", err)
	}
	cmdRel := make([]string, 0, len(cmdFiles))
	for _, name := range cmdFiles {
		cmdRel = append(cmdRel, "cloudflared/cmd/"+name)
	}

	// --------------------------------------------------------- [0] presence
	l.section("[0] 生命周期脚本齐备")
	for _, name := range lifecycleScripts {
		rel := "cloudflared/cmd/" + name
		l.ck(l.exists(rel), name+" 存在")
	}

	// --------------------------------------------------- [1] quote balance
	l.section("[1] 引号与 ${} 展开配平")
	scripts := append([]string{}, cmdRel...)
	for _, extra := range []string{"build.sh", "tools/test-lifecycle.sh"} {
		if l.exists(extra) {
			scripts = append(scripts, extra)
		}
	}
	for _, rel := range scripts {
		text, err := l.read(rel)
		if err != nil {
			l.ck(false, rel+" 可读", err.Error())
			continue
		}
		// Strip comments so an apostrophe in prose is not counted as a quote.
		var code strings.Builder
		for _, line := range shellscan.SplitLines(text) {
			code.WriteString(shellscan.StripComment(line))
			code.WriteString("\n")
		}
		src := code.String()
		singles := strings.Count(src, "'")
		doubles := strings.Count(src, `"`)
		open := strings.Count(src, "${")
		close := strings.Count(src, "}")
		l.ck(singles%2 == 0, rel+": 单引号成对", fmt.Sprintf("count=%d", singles))
		l.ck(doubles%2 == 0, rel+": 双引号成对", fmt.Sprintf("count=%d", doubles))
		l.ck(open <= close, rel+": ${} 展开配平", fmt.Sprintf("open=%d close=%d", open, close))
	}

	// -------------------------------------------------- [2] set -u safety
	l.section("[2] set -u 安全：裸用 ${TRIM_X} 必须有 :- 默认值或同变量守卫")
	var unsafe []string
	for _, rel := range cmdRel {
		text, err := l.read(rel)
		if err != nil {
			continue
		}
		guard := ""
		for i, line := range shellscan.SplitLines(text) {
			if m := guardRE.FindStringSubmatch(line); m != nil {
				guard = m[1]
			}
			if blockEndRE.MatchString(line) {
				guard = ""
			}
			for _, m := range bareTrimRE.FindAllStringSubmatch(line, -1) {
				if m[1] != guard {
					unsafe = append(unsafe, fmt.Sprintf("%s:%d %s", rel, i+1, m[1]))
				}
			}
		}
	}
	l.ck(len(unsafe) == 0, "所有裸用 ${TRIM_X} 都处于 :- 默认值或同变量守卫内", joinLimit(unsafe, 5))

	// --------------------------------------- [3] no interpreter dependency
	l.section("[3] 生命周期脚本不依赖 node/python")
	var offenders []string
	for _, rel := range cmdRel {
		text, err := l.read(rel)
		if err != nil {
			continue
		}
		for _, hit := range shellscan.ScanInterpreter(text) {
			offenders = append(offenders, fmt.Sprintf("%s:%d %s", rel, hit.Line, hit.Name))
		}
	}
	l.ck(len(offenders) == 0,
		"cmd/* 不调用 node/python（服务是静态 Go 二进制，直接 exec）", joinLimit(offenders, 5))

	if l.exists("tools/test-lifecycle.sh") {
		text, _ := l.read("tools/test-lifecycle.sh")
		hits := shellscan.ScanInterpreter(text)
		var detail []string
		for _, hit := range hits {
			detail = append(detail, fmt.Sprintf("%d %s", hit.Line, hit.Name))
		}
		l.ck(len(hits) == 0, "test-lifecycle.sh 不调用 node/python", joinLimit(detail, 5))
	}

	// The shared helper used to probe /var/apps/nodejs_v*/target/bin/node
	// because dependency apps are not added to PATH on fnOS. The payload is
	// self-contained now, so that whole mechanism must be gone -- and if it ever
	// comes back, the "no runtime dependency" claim in the manifest is false.
	if lib, err := l.read("cloudflared/cmd/_lib"); err == nil {
		for _, probe := range []string{"find_node", "nodejs_v", "TRIM_NODE_BIN"} {
			l.ck(!strings.Contains(lib, probe), "_lib: 不再探测解释器运行时（"+probe+"）")
		}
	}

	// ---------------------------------------- [4] the wrapper stays thin
	// build.sh used to implement the whole 12-step pipeline itself, next to a
	// second copy in a PowerShell script; the two drifted. It now only locates
	// Go and hands over to tools/build, so the steps exist in exactly one place.
	l.section("[4] build.sh 只做薄封装")
	if sh, err := l.read("build.sh"); err != nil {
		l.ck(false, "build.sh 可读", err.Error())
	} else {
		l.ck(strings.Contains(sh, "set -euo pipefail"), "build.sh: set -euo pipefail 保留")
		l.ck(strings.Contains(sh, "tools/build"), "build.sh: 转调 tools/build（不再自己实现步骤）")
		l.ck(!pyToolRE.MatchString(sh), "build.sh: 不再调用 tools/*.py 或 *.mjs")
		l.ck(!nodeToolRE.MatchString(sh), "build.sh: 不再需要 node/python")
	}
	// The `bash -n` syntax check on cmd/* moved into the Go pipeline with
	// everything else; assert it is still wired there, because losing it is
	// invisible until the package fails to install.
	if pipeline, err := l.read("tools/build/main.go"); err != nil {
		l.ck(false, "tools/build/main.go 可读", err.Error())
	} else {
		l.ck(strings.Contains(pipeline, "bash") && strings.Contains(pipeline, `"-n"`),
			"tools/build: 仍对 cmd/* 做 bash -n 语法检查")
	}

	// ------------------------------------- [5] uninstall_callback cleanup
	l.section("[5] uninstall_callback：删除数据时清理所有残留")
	if t, err := l.read("cloudflared/cmd/uninstall_callback"); err != nil {
		l.ck(false, "uninstall_callback 可读", err.Error())
	} else {
		// server.log rotates to server.log.1 (up to MaxAppLogBytes = 2MB); the
		// version markers are rewritten on reinstall. Leaving them contradicts
		// the "delete my data" choice in the uninstall wizard.
		for _, target := range []string{"server.log.1", "main.log.1", "VERSION", "cloudflared.version"} {
			l.ck(strings.Contains(t, target), "清理列表包含 "+target)
		}
		l.ck(strings.Contains(t, `rm -f "${PKGVAR}"/*.log`), "通配删除使用 rm -f 且限定在 ${PKGVAR} 内")
		l.ck(strings.Contains(t, "wizard_remove_data"), "仍然由向导选项决定是否删除")
	}

	// -------------------------------------- [6] config_callback merge rules
	l.section("[6] config_callback：读-改-写，只合并 settings")
	if t, err := l.read("cloudflared/cmd/config_callback"); err != nil {
		l.ck(false, "config_callback 可读", err.Error())
	} else {
		l.ck(strings.Contains(t, "config.json"), "config_callback 操作 config.json")
		l.ck(!tunnelsAsgn.MatchString(t), "不整体覆盖 tunnels 字段（否则会抹掉服务写入的隧道）")
		l.ck(!truncateRE.MatchString(t), "不用 cat > config.json 盲写（会丢字段与权限）")
	}

	// --------------------------------------- [7] exit-code contract
	l.section("[7] 生命周期脚本的退出码契约")
	for _, rel := range cmdRel {
		t, err := l.read(rel)
		if err != nil {
			continue
		}
		base := filepath.Base(rel)
		if base == "_lib" {
			// _lib is SOURCED, never executed: a top-level `exit` would
			// terminate whichever script sourced it, at the moment of sourcing.
			// The `exit 1` inside fail() is deliberate -- that function exists to
			// abort the caller with a message the App Center shows.
			var topLevel []string
			for _, line := range shellscan.SplitLines(t) {
				if strings.HasPrefix(line, "exit ") {
					topLevel = append(topLevel, line)
				}
			}
			l.ck(len(topLevel) == 0, "_lib: 没有顶层 exit（它只被 source）", joinLimit(topLevel, 3))
			l.ck(strings.Contains(t, "fail()") && strings.Contains(t, "exit 1"),
				"_lib: fail() 内部保留 exit 1（用于中止调用方）")
			continue
		}
		l.ck(strings.Contains(t, "set -u"), base+": set -u")
		// A script that falls off the end returns the status of its last
		// command, which may be a failed test -- so every script must end
		// explicitly.
		l.ck(hasExplicitExit(t), base+": 显式 exit")
	}

	fmt.Fprintln(out)
	if len(l.failures) > 0 {
		fmt.Fprintf(out, "FAILED: %d passed, %d failed\n", l.passed, len(l.failures))
		for _, f := range l.failures {
			fmt.Fprintf(out, "  - %s\n", f)
		}
		return fmt.Errorf("%d shell lint check(s) failed", len(l.failures))
	}
	fmt.Fprintf(out, "ALL PASSED: %d passed, 0 failed\n", l.passed)
	return nil
}

var exitRE = regexp.MustCompile(`(?m)^\s*exit\s+\d`)

func hasExplicitExit(script string) bool {
	return exitRE.MatchString(script)
}

func listFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

func joinLimit(items []string, limit int) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return strings.Join(items, "; ")
}

// ErrNoScripts is returned when cloudflared/cmd contains nothing to lint.
var ErrNoScripts = errors.New("cloudflared/cmd contains no lifecycle scripts")
