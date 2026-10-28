package cf

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ybd156/fnos-cloudflared/internal/config"
)

func TestLoginStatusIdle(t *testing.T) {
	r, p := newTestRunner(t)

	status := r.LoginStatus()
	if status.LoggedIn {
		t.Error("LoggedIn = true without cert.pem")
	}
	if status.Status != "idle" {
		t.Errorf("Status = %q, want idle", status.Status)
	}
	if status.CertPath != p.CertFile {
		t.Errorf("CertPath = %q, want %q", status.CertPath, p.CertFile)
	}
	if status.URL != "" || status.Error != "" || status.StartedAt != "" {
		t.Errorf("unexpected state: %#v", status)
	}
}

func TestLoginStatusWithCert(t *testing.T) {
	r, p := newTestRunner(t)
	writeCert(t, p)

	status := r.LoginStatus()
	if !status.LoggedIn {
		t.Fatal("LoggedIn = false with cert.pem present")
	}
	// cert.pem on disk always wins over the in-memory state.
	if status.Status != "done" {
		t.Fatalf("Status = %q, want done", status.Status)
	}
}

func TestLogoutClearsCert(t *testing.T) {
	r, p := newTestRunner(t)
	writeCert(t, p)

	r.Logout()

	if status := r.LoginStatus(); status.LoggedIn {
		t.Fatal("cert.pem survived Logout")
	} else if status.Status != "idle" {
		t.Fatalf("Status = %q, want idle", status.Status)
	}
}

func TestListTunnelsNotLoggedIn(t *testing.T) {
	r, _ := newTestRunner(t)
	_, err := r.ListTunnels()
	if err == nil || err.Error() != "尚未登录 Cloudflare 账户" {
		t.Fatalf("error = %v", err)
	}
}

func TestListTunnelsArgvAndParse(t *testing.T) {
	r, p := newTestRunner(t)
	writeCert(t, p)

	var gotArgs []string
	r.execFn = func(args []string, _ ExecOptions) ExecResult {
		gotArgs = args
		return ExecResult{Code: 0, Stdout: `INF Starting
[{"id":"aaa","name":"one","createdAt":"2024-01-02T03:04:05Z","connections":[{"id":"c1"},{"id":"c2"}]},
 {"id":"bbb","name":"two","created_at":"2024-02-03T00:00:00Z"}]`}
	}

	list, err := r.ListTunnels()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// `--output` is a subcommand flag: it has to follow `list`.
	wantArgs := []string{"tunnel", "list", "--output", "json"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
	want := []AccountTunnel{
		{ID: "aaa", Name: "one", CreatedAt: "2024-01-02T03:04:05Z", Connections: 2},
		{ID: "bbb", Name: "two", CreatedAt: "2024-02-03T00:00:00Z", Connections: 0},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("list = %#v, want %#v", list, want)
	}
}

func TestListTunnelsWrappedObject(t *testing.T) {
	r, p := newTestRunner(t)
	writeCert(t, p)
	r.execFn = func([]string, ExecOptions) ExecResult {
		return ExecResult{Code: 0, Stdout: `{"tunnels":[{"id":"ccc","name":"three"}]}`}
	}

	list, err := r.ListTunnels()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(list) != 1 || list[0].ID != "ccc" {
		t.Fatalf("list = %#v", list)
	}
}

func TestListTunnelsUnparseableIsEmpty(t *testing.T) {
	r, p := newTestRunner(t)
	writeCert(t, p)
	r.execFn = func([]string, ExecOptions) ExecResult {
		return ExecResult{Code: 0, Stdout: "no json here"}
	}

	list, err := r.ListTunnels()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("list = %#v, want an empty slice", list)
	}
}

func TestListTunnelsFailureMessage(t *testing.T) {
	r, p := newTestRunner(t)
	writeCert(t, p)
	r.execFn = func([]string, ExecOptions) ExecResult {
		return ExecResult{Code: 1, Stderr: "line1\nline2\nline3\nline4\n"}
	}
	_, err := r.ListTunnels()
	// The last three lines, joined: the actual reason is at the end.
	if err == nil || err.Error() != "line2 line3 line4" {
		t.Fatalf("error = %v", err)
	}

	r.execFn = func([]string, ExecOptions) ExecResult {
		return ExecResult{Code: 3}
	}
	if _, err := r.ListTunnels(); err == nil || err.Error() != "cloudflared 退出码 3" {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateTunnel(t *testing.T) {
	r, _ := newTestRunner(t)

	if _, _, err := r.CreateTunnel("x"); err == nil || err.Error() != "尚未登录 Cloudflare 账户" {
		t.Fatalf("not logged in error = %v", err)
	}

	r, p := newTestRunner(t)
	writeCert(t, p)

	if _, _, err := r.CreateTunnel("bad name!"); err == nil ||
		err.Error() != "隧道名称只能包含字母、数字、点、下划线和连字符" {
		t.Fatalf("invalid name error = %v", err)
	}

	var gotArgs []string
	r.execFn = func(args []string, _ ExecOptions) ExecResult {
		gotArgs = args
		return ExecResult{Code: 0, Stdout: "INF created\n{\"id\":\"new-id\",\"name\":\"nas\"}\n"}
	}

	raw, output, err := r.CreateTunnel("nas")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantArgs := []string{"tunnel", "create", "--output", "json", "nas"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
	if string(raw) != `{"id":"new-id","name":"nas"}` {
		t.Fatalf("raw = %s", raw)
	}
	if !json.Valid(raw) {
		t.Fatal("raw is not valid JSON")
	}
	if output != "INF created\n{\"id\":\"new-id\",\"name\":\"nas\"}" {
		t.Fatalf("output = %q", output)
	}
}

func TestCreateTunnelFailure(t *testing.T) {
	r, p := newTestRunner(t)
	writeCert(t, p)
	r.execFn = func([]string, ExecOptions) ExecResult {
		return ExecResult{Code: 1, Stderr: "a\nb\nc\nd"}
	}
	raw, output, err := r.CreateTunnel("nas")
	if err == nil || err.Error() != "b c d" {
		t.Fatalf("error = %v", err)
	}
	if raw != nil {
		t.Fatalf("raw = %s, want nil", raw)
	}
	if !strings.Contains(output, "a") {
		t.Fatalf("output = %q", output)
	}
}

func TestDeleteTunnel(t *testing.T) {
	r, _ := newTestRunner(t)
	if err := r.DeleteTunnel("x"); err == nil || err.Error() != "尚未登录 Cloudflare 账户" {
		t.Fatalf("not logged in error = %v", err)
	}

	r, p := newTestRunner(t)
	writeCert(t, p)
	if err := r.DeleteTunnel("../etc"); err == nil || err.Error() != "非法的隧道名称" {
		t.Fatalf("invalid name error = %v", err)
	}

	var gotArgs []string
	r.execFn = func(args []string, _ ExecOptions) ExecResult {
		gotArgs = args
		return ExecResult{Code: 0}
	}
	if err := r.DeleteTunnel("nas"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// -f skips the interactive confirmation.
	if want := []string{"tunnel", "delete", "-f", "nas"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args = %#v, want %#v", gotArgs, want)
	}
}

func TestRouteDNS(t *testing.T) {
	r, _ := newTestRunner(t)
	if err := r.RouteDNS("nas", "a.example.com"); err == nil ||
		err.Error() != "尚未登录 Cloudflare 账户" {
		t.Fatalf("not logged in error = %v", err)
	}

	r, p := newTestRunner(t)
	writeCert(t, p)

	if err := r.RouteDNS("bad name", "a.example.com"); err == nil ||
		err.Error() != "非法的隧道名称" {
		t.Fatalf("invalid name error = %v", err)
	}
	for _, bad := range []string{"", "http://a.example.com", "a.example.com/x", "localhost"} {
		if err := r.RouteDNS("nas", bad); err == nil || err.Error() != "非法的域名" {
			t.Fatalf("hostname %q error = %v", bad, err)
		}
	}

	var gotArgs []string
	r.execFn = func(args []string, _ ExecOptions) ExecResult {
		gotArgs = args
		return ExecResult{Code: 0, Stdout: "ok"}
	}
	if err := r.RouteDNS("nas", "*.example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// --overwrite-dns replaces a stale A record instead of refusing.
	want := []string{"tunnel", "route", "dns", "--overwrite-dns", "nas", "*.example.com"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args = %#v, want %#v", gotArgs, want)
	}
}

func TestResolveTunnelIDByName(t *testing.T) {
	r, p := newTestRunner(t)
	if got := r.ResolveTunnelIDByName(""); got != "" {
		t.Fatalf("empty name = %q", got)
	}
	// Not logged in: best effort, no error, no id.
	if got := r.ResolveTunnelIDByName("nas"); got != "" {
		t.Fatalf("not logged in = %q, want \"\"", got)
	}

	writeCert(t, p)
	r.execFn = func([]string, ExecOptions) ExecResult {
		return ExecResult{Code: 0, Stdout: `[{"id":"` + testUUID + `","name":"nas"}]`}
	}
	if got := r.ResolveTunnelIDByName("nas"); got != testUUID {
		t.Fatalf("resolved = %q, want %q", got, testUUID)
	}
	if got := r.ResolveTunnelIDByName("other"); got != "" {
		t.Fatalf("unknown name = %q, want \"\"", got)
	}
}

func TestTailLines(t *testing.T) {
	if got := tailLines("", 3); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := tailLines("a\nb", 3); got != "a b" {
		t.Fatalf("two lines = %q", got)
	}
	if got := tailLines("1\n2\n3\n4\n5", 3); got != "3 4 5" {
		t.Fatalf("tail = %q", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x"); got != "x" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestJSONHelpers(t *testing.T) {
	// cloudflared prints log lines before the payload.
	body := "INF x\n{\"a\":1}"
	if idx := jsonStart(body); idx != len("INF x\n") {
		t.Fatalf("jsonStart = %d", idx)
	}
	if got := string(parseJSONBody(body)); got != `{"a":1}` {
		t.Fatalf("parseJSONBody = %q", got)
	}
	if got := parseJSONRaw(body); string(got) != `{"a":1}` {
		t.Fatalf("parseJSONRaw = %s", got)
	}
	if got := parseJSONRaw("nothing"); got != nil {
		t.Fatalf("parseJSONRaw(no json) = %s, want nil", got)
	}
	// Truncated JSON must not be forwarded as an object.
	if got := parseJSONRaw(`{"a":`); got != nil {
		t.Fatalf("parseJSONRaw(truncated) = %s, want nil", got)
	}
	// An array starts before an object.
	if idx := jsonStart(`[{"a":1}]`); idx != 0 {
		t.Fatalf("array jsonStart = %d", idx)
	}
}

func TestCredentialFileName(t *testing.T) {
	r, p := newTestRunner(t)
	// The credentials file lives in CFDir and is named after the UUID.
	if got, want := p.CredentialFile(testUUID), r.p.CredentialFile(testUUID); got != want {
		t.Fatalf("CredentialFile = %q, want %q", got, want)
	}
	if !strings.HasSuffix(r.p.CredentialFile(testUUID), testUUID+".json") {
		t.Fatalf("CredentialFile = %q", r.p.CredentialFile(testUUID))
	}
	// The UUID pattern accepts the file stem, which is what dnsroute relies on.
	if !config.UUIDRE.MatchString(testUUID) {
		t.Fatal("testUUID does not match config.UUIDRE")
	}
}
