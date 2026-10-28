package lintui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a minimal but complete frontend under <tmp>/cloudflared/app/www.
func fixture(t *testing.T, indexHTML, css string, js map[string]string) string {
	t.Helper()
	root := t.TempDir()
	www := filepath.Join(root, "cloudflared", "app", "www")
	if err := os.MkdirAll(filepath.Join(www, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(www, "index.html"), indexHTML)
	write(filepath.Join(www, "style.css"), css)
	for rel, body := range js {
		write(filepath.Join(www, filepath.FromSlash(rel)), body)
	}
	return root
}

const goodIndex = `<!doctype html>
<main id="view-tunnels"><ul class="tunnel-list" id="tunnel-list"></ul></main>
<div class="toast hidden" id="toast"></div>
<div class="modal-backdrop hidden" id="modal-backdrop"></div>
`

// goodCSS must define every class the checks require, including the dynamic
// state tokens that only appear through a template interpolation.
const goodCSS = `.tunnel-list{list-style:none}
.toast{position:fixed}
.hidden{display:none}
.modal-backdrop{position:fixed}
.tunnel-card{border:1px solid}
.status{font-weight:600}
.running{color:green}
.starting{color:amber}
.stopping{color:orange}
.stopped{color:grey}
.error{color:red}
.is-running{color:green}
.is-starting{color:amber}
.is-stopping{color:orange}
.is-stopped{color:grey}
.is-error{color:red}
.ok{color:green}
.err{color:red}
.info{color:blue}
.warn{color:orange}
`

// The JS lives in a subdirectory and in several files: the whole point of the
// multi-file scan is that a selector may be used from any module.
var goodJS = map[string]string{
	"js/app.js": "const list = $('#tunnel-list');\n$('#toast').classList.add('hidden');\n",
	"js/render.js": "function card(status) {\n" +
		"  return `<div class=\"tunnel-card ${status}\"><span class=\"status\">x</span></div>`;\n" +
		"}\n",
}

func TestRunPasses(t *testing.T) {
	root := fixture(t, goodIndex, goodCSS, goodJS)
	var out bytes.Buffer
	if err := Run(root, &out); err != nil {
		t.Fatalf("unexpected failure: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "no unresolved UI selectors") {
		t.Fatalf("missing success line:\n%s", out.String())
	}
}

func TestUnknownSelectorFails(t *testing.T) {
	js := map[string]string{
		"js/app.js": "const x = $('#modal-bodi');\n",
	}
	root := fixture(t, goodIndex, goodCSS, js)
	var out bytes.Buffer
	err := Run(root, &out)
	if err == nil {
		t.Fatal("a selector that does not resolve was accepted")
	}
	if !strings.Contains(out.String(), "modal-bodi") {
		t.Fatalf("the failure does not name the selector:\n%s", out.String())
	}
}

func TestSelectorUsedFromAnyModuleIsFound(t *testing.T) {
	// The id is only referenced from a nested module, never from the entry.
	js := map[string]string{
		"js/app.js":        "export const start = () => {};\n",
		"js/views/deep.js": "document.getElementById('not-there');\n",
	}
	root := fixture(t, goodIndex, goodCSS, js)
	var out bytes.Buffer
	if err := Run(root, &out); err == nil {
		t.Fatalf("a selector in a nested module was not scanned\n%s", out.String())
	}
}

func TestDuplicateIDFails(t *testing.T) {
	index := goodIndex + `<div id="toast"></div>`
	root := fixture(t, index, goodCSS, goodJS)
	var out bytes.Buffer
	if err := Run(root, &out); err == nil {
		t.Fatal("a duplicate id was accepted")
	}
	if !strings.Contains(out.String(), "必须唯一") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

func TestUnstyledClassFails(t *testing.T) {
	js := map[string]string{
		"js/app.js": `<div class="mystery-box"></div>`,
	}
	root := fixture(t, goodIndex, goodCSS, js)
	var out bytes.Buffer
	if err := Run(root, &out); err == nil {
		t.Fatal("a class without a style.css rule was accepted")
	}
	if !strings.Contains(out.String(), "mystery-box") {
		t.Fatalf("the failure does not name the class:\n%s", out.String())
	}
}

// A template interpolation may contain quotes; the class scanner must not stop
// at them and leak fragments such as `${state.filter` into the class list.
func TestInterpolatedClassWithQuotesIsIgnored(t *testing.T) {
	js := map[string]string{
		"js/render.js": "`<button class=\"chip ${state.filter === 'all' ? 'is-active' : ''}\">x</button>`",
	}
	css := goodCSS + ".chip{color:red}\n"
	root := fixture(t, goodIndex, css, js)
	var out bytes.Buffer
	if err := Run(root, &out); err != nil {
		t.Fatalf("an interpolated class expression was misparsed: %v\n%s", err, out.String())
	}
}

func TestToggledClassWithoutRuleFails(t *testing.T) {
	js := map[string]string{
		"js/app.js": "$('#toast').classList.toggle('is-invisible');\n",
	}
	root := fixture(t, goodIndex, goodCSS, js)
	var out bytes.Buffer
	if err := Run(root, &out); err == nil {
		t.Fatal("a classList toggle without a CSS rule was accepted")
	}
}

func TestNoJSFilesFails(t *testing.T) {
	root := fixture(t, goodIndex, goodCSS, nil)
	var out bytes.Buffer
	if err := Run(root, &out); err == nil {
		t.Fatal("a frontend without any .js file was accepted")
	}
}
