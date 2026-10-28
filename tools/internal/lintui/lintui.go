// Package lintui checks the web UI's selector contract.
//
// The UI is plain DOM scripting with no build step, so nothing catches a
// selector that points at an element which does not exist. A typo there fails
// silently: the handler is attached to null (or to nothing), the button simply
// does nothing, and no error is reported anywhere.
//
// Port of tools/lint-ui.mjs, with one deliberate difference: the frontend is
// being split into several ES modules under www/, so every .js file below www/
// is scanned instead of only app.js. Scanning one file would have made the
// contract quietly weaker the moment the split landed.
//
// Three checks, each of which corresponds to a real way the UI can go dead:
//
//  1. Every '#id' the JS looks up must exist -- either in index.html or in the
//     markup the JS generates itself (the modal body and the tunnel list are
//     built at runtime).
//  2. Every static class token used in markup must have a rule in style.css,
//     otherwise the intended styling silently does not apply.
//  3. Every id in index.html must be unique.
package lintui

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// idAttrRE finds an id="..." / id='...' attribute in markup. A word
	// boundary would also match data-id=..., so require start-of-line or
	// whitespace before it.
	idAttrRE = regexp.MustCompile(`(?m)(?:^|[\s])id\s*=\s*"([A-Za-z][\w-]*)"|(?:^|[\s])id\s*=\s*'([A-Za-z][\w-]*)'`)
	// refDollarRE finds $('#id') / $('#id', root) lookups.
	refDollarRE = regexp.MustCompile(`\$\(\s*['"]#([A-Za-z][\w-]*)['"]\s*[,)]`)
	// refGetByIDRE finds document.getElementById('id').
	refGetByIDRE = regexp.MustCompile(`getElementById\(\s*['"]([A-Za-z][\w-]*)['"]\s*[,)]`)
	// refQueryRE finds querySelector('#id').
	refQueryRE = regexp.MustCompile(`querySelector\(\s*['"]#([A-Za-z][\w-]*)['"]\s*[,)]`)

	// A class attribute must be matched per quote character: an expression such
	// as class="chip ${state.filter === 'all' ? 'is-active' : ''}" contains
	// single quotes, so a single "any quote" pattern would stop early and leak
	// fragments like `${state.filter` into the class list.
	classDoubleRE = regexp.MustCompile(`\bclass\s*=\s*"([^"]*)"`)
	classSingleRE = regexp.MustCompile(`\bclass\s*=\s*'([^']*)'`)
	interpRE      = regexp.MustCompile(`\$\{[^}]*\}`)
	classListRE   = regexp.MustCompile(`classList\.(?:add|remove|toggle)\(\s*['"]([\w-]+)['"]`)
	cssCommentRE  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssSelectorRE = regexp.MustCompile(`\.(-?[A-Za-z_][\w-]*)`)
)

// dynamicValues are the class tokens that only ever appear through a template
// interpolation (`class="status ${status}"`, `class="tunnel-card is-${status}"`),
// so their concrete spelling cannot be read out of the markup. They are listed
// explicitly and checked like any other token.
var dynamicValues = []string{
	"running", "starting", "stopping", "stopped", "error",
	"is-running", "is-starting", "is-stopping", "is-stopped", "is-error",
	"ok", "err", "info", "warn",
}

// Run lints the frontend of the checkout at root.
func Run(root string, out io.Writer) error {
	www := filepath.Join(root, "cloudflared", "app", "www")

	indexHTML, err := readFile(filepath.Join(www, "index.html"))
	if err != nil {
		return fmt.Errorf("cannot read index.html: %w", err)
	}
	css, err := readFile(filepath.Join(www, "style.css"))
	if err != nil {
		return fmt.Errorf("cannot read style.css: %w", err)
	}
	jsFiles, err := findJS(www)
	if err != nil {
		return err
	}
	if len(jsFiles) == 0 {
		return fmt.Errorf("no .js files under %s: the UI cannot work without them", filepath.ToSlash(www))
	}

	var js strings.Builder
	for _, f := range jsFiles {
		src, err := readFile(f)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", filepath.ToSlash(f), err)
		}
		js.WriteString(src)
		js.WriteString("\n")
	}
	jsSrc := js.String()

	var problems []string
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		problems = append(problems, msg)
		fmt.Fprintf(out, "FAIL  %s\n", msg)
	}

	// ── 1. ids ───────────────────────────────────────────────────────────
	htmlIDs := idSet(indexHTML)

	// Duplicate ids: getElementById returns the first, so the second element is
	// unreachable and any handler bound to it is bound to the wrong node.
	counts := make(map[string]int)
	for _, id := range allIDs(indexHTML) {
		counts[id]++
	}
	for _, id := range sortedKeys(counts) {
		if counts[id] > 1 {
			fail("index.html 中 id=%q 出现了 %d 次（必须唯一）", id, counts[id])
		}
	}

	// Ids the JS renders into the DOM itself. Only the modal body and the
	// tunnel list are built at runtime, so scanning every module is close
	// enough -- an id string that appears anywhere in the JS is available by
	// the time it is queried, and a false negative here would only ever hide a
	// genuine typo if the same id happened to appear in unrelated text.
	generatedIDs := idSet(jsSrc)

	referenced := make(map[string]bool)
	for _, re := range []*regexp.Regexp{refDollarRE, refGetByIDRE, refQueryRE} {
		for _, m := range re.FindAllStringSubmatch(jsSrc, -1) {
			referenced[m[1]] = true
		}
	}
	for _, id := range sortedSet(referenced) {
		if !htmlIDs[id] && !generatedIDs[id] {
			fail("前端查询了 #%s，但 index.html 和 JS 生成的标记里都没有这个 id", id)
		}
	}

	// ── 2. classes ───────────────────────────────────────────────────────
	used := make(map[string]bool)
	for _, src := range []string{indexHTML, jsSrc} {
		for _, m := range classTokens(src) {
			// `class="tunnel-card is-${status}"` splits into the literal parts
			// `tunnel-card` and `is-`. The trailing '-' marks a prefix that the
			// interpolation completes, so it is not a class name of its own --
			// the concrete values are covered by dynamicValues below.
			withoutExpr := interpRE.ReplaceAllString(m, " ")
			for _, token := range strings.Fields(withoutExpr) {
				if !strings.HasSuffix(token, "-") {
					used[token] = true
				}
			}
		}
	}
	for _, v := range dynamicValues {
		used[v] = true
	}

	// A class is "styled" if it appears anywhere in the stylesheet as a
	// selector. Comments are stripped first so a class named only in prose does
	// not count.
	styled := make(map[string]bool)
	for _, m := range cssSelectorRE.FindAllStringSubmatch(cssCommentRE.ReplaceAllString(css, " "), -1) {
		styled[m[1]] = true
	}

	for _, c := range sortedSet(used) {
		if !styled[c] {
			fail("标记里用了 .%s，但 style.css 里没有对应规则（样式不会生效）", c)
		}
	}

	// ── 3. classes the JS toggles but CSS never defines ──────────────────
	for _, m := range classListRE.FindAllStringSubmatch(jsSrc, -1) {
		if !styled[m[1]] {
			fail("JS 切换了 .%s，但 style.css 里没有这条规则", m[1])
		}
	}

	if len(problems) > 0 {
		fmt.Fprintf(out, "\n%d 个 UI 选择器问题。\n", len(problems))
		return fmt.Errorf("%d UI selector problem(s)", len(problems))
	}
	fmt.Fprintf(out, "ok    index.html %d 个 id，%d 个类均有样式，选择器全部可解析（扫描 %d 个 .js）\n",
		len(htmlIDs), len(used), len(jsFiles))
	fmt.Fprintln(out, "\nno unresolved UI selectors")
	return nil
}

// findJS returns every .js file under dir, sorted, including nested module
// directories.
func findJS(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".js") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func idSet(src string) map[string]bool {
	out := make(map[string]bool)
	for _, id := range allIDs(src) {
		out[id] = true
	}
	return out
}

// allIDs returns every id="..." / id='...' value in order, including repeats.
func allIDs(src string) []string {
	var out []string
	for _, m := range idAttrRE.FindAllStringSubmatch(src, -1) {
		if m[1] != "" {
			out = append(out, m[1])
		} else if m[2] != "" {
			out = append(out, m[2])
		}
	}
	return out
}

// classTokens returns the raw value of every class attribute, whichever quote
// character delimits it.
func classTokens(src string) []string {
	var out []string
	for _, m := range classDoubleRE.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	for _, m := range classSingleRE.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
