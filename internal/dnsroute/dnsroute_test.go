package dnsroute

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/cf"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

const testUUID = "0f5f1a2b-3c4d-5e6f-7a8b-9c0d1e2f3a4b"

func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "apps", "cloudflared")
	p := paths.Paths{
		AppName: "cloudflared",
		AppDest: filepath.Join(base, "target"),
		PkgVar:  filepath.Join(base, "var"),
		PkgTmp:  filepath.Join(base, "tmp"),
	}
	p.CFBin = filepath.Join(p.AppDest, "bin", "cloudflared")
	p.CFDir = filepath.Join(p.PkgVar, ".cloudflared")
	p.CFHome = p.PkgVar
	p.CertFile = filepath.Join(p.CFDir, "cert.pem")
	p.TunnelsDir = filepath.Join(p.PkgVar, "tunnels")
	p.LogsDir = filepath.Join(p.PkgVar, "logs")
	p.AppLog = filepath.Join(p.PkgVar, "server.log")
	return p
}

// newTestResolver returns a resolver whose clock the test drives.
func newTestResolver(t *testing.T) (*Resolver, paths.Paths) {
	t.Helper()
	p := testPaths(t)
	fsx.MkdirAll(p.PkgVar, 0o755)
	log := applog.New(p.AppLog)
	log.Out = io.Discard
	log.Err = io.Discard
	t.Cleanup(log.Close)

	res := New(p, log, cf.New(p, log))
	now := time.Date(2026, 10, 28, 5, 42, 49, 0, time.UTC)
	res.now = func() time.Time { return now }
	return res, p
}

func writeCert(t *testing.T, p paths.Paths) {
	t.Helper()
	fsx.MkdirAll(p.CFDir, 0o700)
	if err := os.WriteFile(p.CertFile, []byte("cert"), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
}

// dohServer answers every query with the given CNAME target (or no answer at
// all when target is empty).
func dohServer(t *testing.T, res *Resolver, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("type"); got != "CNAME" {
			t.Errorf("type = %q, want CNAME", got)
		}
		if r.Header.Get("accept") != "application/dns-json" {
			t.Errorf("accept = %q", r.Header.Get("accept"))
		}
		w.Header().Set("Content-Type", "application/dns-json")
		if target == "" {
			_, _ = io.WriteString(w, `{"Status":0,"Answer":[]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Status": 0,
			"Answer": []map[string]any{
				// A decoy A record first: only type 5 counts.
				{"name": r.URL.Query().Get("name"), "type": 1, "data": "203.0.113.7"},
				{"name": r.URL.Query().Get("name"), "type": 5, "data": target},
			},
		})
	}))
	t.Cleanup(srv.Close)
	res.dohEndpoint = srv.URL
	return srv
}

func localTunnel() config.Tunnel {
	return config.Tunnel{
		ID:         "t_local",
		Type:       config.TypeLocal,
		TunnelName: "nas",
		TunnelID:   testUUID,
		AutoDNS:    true,
		Ingress: []config.Ingress{
			{Hostname: "nas.example.com", Service: "http://127.0.0.1:5000"},
			{Hostname: "web.example.com", Service: "http://127.0.0.1:5001"},
		},
	}
}

func TestHostnames(t *testing.T) {
	res, _ := newTestResolver(t)

	tun := config.Tunnel{Ingress: []config.Ingress{
		{Hostname: "a.example.com"},
		{Hostname: "  a.example.com  "}, // duplicate after trimming
		{Hostname: ""},
		{Hostname: "http://b.example.com"}, // not a bare hostname
		{Hostname: "c.example.com:8080"},
		{Hostname: "*.wild.example.com"},
		{Hostname: "d.example.com/path"},
	}}
	want := []string{"a.example.com", "*.wild.example.com"}
	if got := res.Hostnames(tun); !reflect.DeepEqual(got, want) {
		t.Fatalf("Hostnames = %#v, want %#v", got, want)
	}
}

func TestUUID(t *testing.T) {
	res, p := newTestResolver(t)

	// No credentials file: no UUID. Never guess.
	if got := res.UUID(localTunnel()); got != "" {
		t.Fatalf("UUID without credentials = %q", got)
	}

	fsx.MkdirAll(p.CFDir, 0o700)
	cred := p.CredentialFile(testUUID)
	if err := os.WriteFile(cred, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The UUID is recovered from the file name, but only through a provable
	// path: an explicit credentials file (a name alone proves nothing).
	if got := res.UUID(config.Tunnel{CredentialsFile: cred}); got != testUUID {
		t.Fatalf("UUID = %q, want %q", got, testUUID)
	}
	if got := res.UUID(config.Tunnel{Type: config.TypeLocal, TunnelName: "nas"}); got != "" {
		t.Fatalf("name-only UUID = %q, want \"\"", got)
	}

	// A credentials file whose stem is not a UUID proves nothing.
	odd := filepath.Join(p.CFDir, "backup.json")
	if err := os.WriteFile(odd, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := res.UUID(config.Tunnel{CredentialsFile: odd}); got != "" {
		t.Fatalf("UUID from non-uuid file = %q", got)
	}
}

func TestCacheFresh(t *testing.T) {
	res, _ := newTestResolver(t)
	base := time.Date(2026, 10, 28, 5, 42, 49, 0, time.UTC)
	now := base
	res.now = func() time.Time { return now }

	if res.CacheFresh("nas", "nas.example.com") {
		t.Fatal("empty cache reported fresh")
	}

	res.Remember("nas", "nas.example.com", testUUID+".cfargotunnel.com")
	if !res.CacheFresh("nas", "nas.example.com") {
		t.Fatal("just-remembered route not fresh")
	}

	// Exactly at the ten minute mark it is still trusted.
	now = base.Add(10 * time.Minute)
	if !res.CacheFresh("nas", "nas.example.com") {
		t.Fatal("route not fresh at exactly 10 minutes")
	}
	now = base.Add(10*time.Minute + time.Second)
	if res.CacheFresh("nas", "nas.example.com") {
		t.Fatal("route still fresh after 10 minutes")
	}

	// The key is the name/hostname pair, not the hostname alone.
	now = base
	if res.CacheFresh("other", "nas.example.com") {
		t.Fatal("cache key ignored the tunnel name")
	}
	if res.CacheFresh("nas", "web.example.com") {
		t.Fatal("cache key ignored the hostname")
	}
}

func TestEnsureSkipsNonLocalTunnels(t *testing.T) {
	res, _ := newTestResolver(t)

	for _, tun := range []config.Tunnel{
		{Type: config.TypeToken, AutoDNS: true},
		{Type: config.TypeQuick, AutoDNS: true},
		{Type: config.TypeConfig, AutoDNS: true},
	} {
		got := res.Ensure(tun)
		if !got.OK || !got.Skipped {
			t.Fatalf("type %q: OK=%v Skipped=%v, want true/true", tun.Type, got.OK, got.Skipped)
		}
		if got.Message != "未启用自动 DNS 路由" {
			t.Fatalf("type %q message = %q", tun.Type, got.Message)
		}
		if got.Routed == nil || got.AlreadyOK == nil || got.Failed == nil {
			t.Fatalf("type %q: result slices must be non-nil", tun.Type)
		}
	}

	// autoDns off is also skipped.
	tun := localTunnel()
	tun.AutoDNS = false
	if got := res.Ensure(tun); !got.Skipped || got.Message != "未启用自动 DNS 路由" {
		t.Fatalf("autoDns=false: %#v", got)
	}
}

func TestEnsurePreconditions(t *testing.T) {
	res, p := newTestResolver(t)

	tun := localTunnel()
	tun.TunnelName = ""
	tun.TunnelID = ""
	if got := res.Ensure(tun); got.OK || got.Message != "未选择已创建的隧道" {
		t.Fatalf("no tunnel: %#v", got)
	}

	tun = localTunnel()
	tun.Ingress = nil
	if got := res.Ensure(tun); !got.OK || got.Message != "入站规则中没有需要路由的域名" {
		t.Fatalf("no hostnames: %#v", got)
	}

	if got := res.Ensure(localTunnel()); got.OK ||
		got.Message != "尚未登录 Cloudflare 账户，无法自动配置 DNS 路由" {
		t.Fatalf("not logged in: %#v", got)
	}

	writeCert(t, p)
	tun = localTunnel()
	tun.TunnelID = ""
	if got := res.Ensure(tun); got.OK || got.Skipped ||
		got.Message != "无法确认隧道 UUID，跳过 DNS 路由配置" {
		t.Fatalf("no uuid: %#v", got)
	}
}

func TestEnsureAlreadyOK(t *testing.T) {
	res, p := newTestResolver(t)
	writeCert(t, p)
	dohServer(t, res, testUUID+".cfargotunnel.com.")

	calls := 0
	res.routeFn = func(name, hostname string) error {
		calls++
		return nil
	}

	got := res.Ensure(localTunnel())
	if !got.OK {
		t.Fatalf("OK = false: %#v", got)
	}
	if got.Skipped {
		t.Fatal("Skipped = true after the checks passed")
	}
	if calls != 0 {
		t.Fatalf("route called %d times for already-correct records", calls)
	}
	want := []string{"nas.example.com", "web.example.com"}
	if !reflect.DeepEqual(got.AlreadyOK, want) {
		t.Fatalf("AlreadyOK = %#v, want %#v", got.AlreadyOK, want)
	}
	if got.Message != "DNS 路由已正确: nas.example.com、web.example.com" {
		t.Fatalf("message = %q", got.Message)
	}
	// A verified route is cached, which is what lets the caller skip a re-check.
	if !res.CacheFresh("nas", "nas.example.com") {
		t.Fatal("verified route was not remembered")
	}
}

func TestEnsureCreatesRoutes(t *testing.T) {
	res, p := newTestResolver(t)
	writeCert(t, p)
	dohServer(t, res, "") // no CNAME yet

	var routed []string
	res.routeFn = func(name, hostname string) error {
		routed = append(routed, name+"|"+hostname)
		return nil
	}

	got := res.Ensure(localTunnel())
	if !got.OK || got.Skipped {
		t.Fatalf("unexpected result: %#v", got)
	}
	wantRouted := []string{"nas|nas.example.com", "nas|web.example.com"}
	if !reflect.DeepEqual(routed, wantRouted) {
		t.Fatalf("route calls = %#v, want %#v", routed, wantRouted)
	}
	if got.Message != "已配置 DNS 路由: nas.example.com、web.example.com" {
		t.Fatalf("message = %q", got.Message)
	}
	if !res.CacheFresh("nas", "nas.example.com") {
		t.Fatal("created route was not remembered")
	}
}

func TestEnsureRepairsWrongTarget(t *testing.T) {
	res, p := newTestResolver(t)
	writeCert(t, p)
	// An A-record-only name (or a CNAME pointing somewhere else) must be
	// overwritten, not accepted.
	dohServer(t, res, "somewhere-else.example.com.")

	var routed []string
	res.routeFn = func(name, hostname string) error {
		routed = append(routed, hostname)
		return nil
	}

	got := res.Ensure(localTunnel())
	if len(routed) != 2 {
		t.Fatalf("route calls = %#v, want both hostnames", routed)
	}
	if len(got.AlreadyOK) != 0 {
		t.Fatalf("AlreadyOK = %#v, want empty", got.AlreadyOK)
	}
}

func TestEnsureFailures(t *testing.T) {
	res, p := newTestResolver(t)
	writeCert(t, p)
	dohServer(t, res, "")

	res.routeFn = func(name, hostname string) error {
		if hostname == "web.example.com" {
			return errors.New("API 拒绝了请求")
		}
		return nil
	}

	got := res.Ensure(localTunnel())
	if got.OK {
		t.Fatal("OK = true with a failed route")
	}
	if len(got.Routed) != 1 || got.Routed[0] != "nas.example.com" {
		t.Fatalf("Routed = %#v", got.Routed)
	}
	if len(got.Failed) != 1 {
		t.Fatalf("Failed = %#v", got.Failed)
	}
	if got.Failed[0].Hostname != "web.example.com" || got.Failed[0].Error != "API 拒绝了请求" {
		t.Fatalf("Failed[0] = %#v", got.Failed[0])
	}
	if got.Message != "web.example.com: API 拒绝了请求" {
		t.Fatalf("message = %q", got.Message)
	}
}

func TestEnsureToleratesLookupFailure(t *testing.T) {
	res, p := newTestResolver(t)
	writeCert(t, p)

	// A broken DoH endpoint must not abort the run: the route is still created.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	res.dohEndpoint = srv.URL

	res.routeFn = func(name, hostname string) error { return nil }

	got := res.Ensure(localTunnel())
	if !got.OK {
		t.Fatalf("OK = false: %#v", got)
	}
	if len(got.Routed) != 2 {
		t.Fatalf("Routed = %#v", got.Routed)
	}
}

func TestLookupCNAME(t *testing.T) {
	res, _ := newTestResolver(t)

	dohServer(t, res, "target.example.com.")
	got, err := res.LookupCNAME("nas.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "target.example.com" {
		t.Fatalf("LookupCNAME = %q, want target.example.com (trailing dot removed)", got)
	}

	// No CNAME answer at all is "" with no error: that means "needs a route".
	dohServer(t, res, "")
	got, err = res.LookupCNAME("nas.example.com")
	if err != nil || got != "" {
		t.Fatalf("no-answer lookup = %q, %v", got, err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	res.dohEndpoint = srv.URL
	if _, err := res.LookupCNAME("nas.example.com"); err == nil ||
		err.Error() != "DNS 查询失败 (HTTP 502)" {
		t.Fatalf("HTTP failure error = %v", err)
	}
}

func TestLookupCNAMEBadJSON(t *testing.T) {
	res, _ := newTestResolver(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	t.Cleanup(srv.Close)
	res.dohEndpoint = srv.URL

	_, err := res.LookupCNAME("nas.example.com")
	if err == nil || !strings.HasPrefix(err.Error(), "DNS 查询失败") {
		t.Fatalf("error = %v", err)
	}
}

func TestResultJSONShape(t *testing.T) {
	res, _ := newTestResolver(t)
	raw, err := json.Marshal(res.Ensure(config.Tunnel{Type: config.TypeToken}))
	if err != nil {
		t.Fatal(err)
	}
	// The UI reads these exact keys.
	for _, key := range []string{`"ok":`, `"routed":`, `"alreadyOk":`, `"failed":`, `"skipped":`, `"message":`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("result JSON missing %s: %s", key, raw)
		}
	}
	if !strings.Contains(string(raw), `"routed":[]`) {
		t.Fatalf("empty slices must encode as [], got %s", raw)
	}
}

func TestFailureJSONShape(t *testing.T) {
	raw, err := json.Marshal(Failure{Hostname: "a.example.com", Error: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"hostname":"a.example.com","error":"boom"}` {
		t.Fatalf("Failure JSON = %s", raw)
	}
}

func TestEnsureRecoversUUIDFromCredentialsFile(t *testing.T) {
	res, p := newTestResolver(t)
	writeCert(t, p)
	fsx.MkdirAll(p.CFDir, 0o700)
	cred := p.CredentialFile(testUUID)
	if err := os.WriteFile(cred, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotName string
	dohServer(t, res, testUUID+".cfargotunnel.com.")
	res.routeFn = func(name, hostname string) error {
		gotName = name
		return nil
	}

	// No stored id, but an explicit credentials file: the UUID comes from its
	// name and the existing routes verify against it.
	tun := localTunnel()
	tun.TunnelID = ""
	tun.CredentialsFile = cred
	got := res.Ensure(tun)
	if !got.OK || got.Skipped {
		t.Fatalf("unexpected result: %#v", got)
	}
	if len(got.AlreadyOK) != 2 {
		t.Fatalf("AlreadyOK = %#v", got.AlreadyOK)
	}
	if gotName != "" {
		t.Fatalf("route called for an already-correct record: %q", gotName)
	}
}

func TestEnsureEmptyUUIDTargetIsNotGuessed(t *testing.T) {
	res, p := newTestResolver(t)
	writeCert(t, p)
	// A DoH answer of ".cfargotunnel.com" (the broken target the previous
	// implementation warned about) must never be treated as a match.
	dohServer(t, res, ".cfargotunnel.com.")
	res.routeFn = func(name, hostname string) error { return nil }

	tun := localTunnel()
	tun.TunnelID = ""
	got := res.Ensure(tun)
	if got.OK {
		t.Fatalf("OK = true without a UUID: %#v", got)
	}
	if got.Message != "无法确认隧道 UUID，跳过 DNS 路由配置" {
		t.Fatalf("message = %q", got.Message)
	}
}

func TestRememberOverwrites(t *testing.T) {
	res, _ := newTestResolver(t)
	res.Remember("nas", "a.example.com", "one")
	res.Remember("nas", "a.example.com", "two")

	res.mu.Lock()
	entry := res.cache[cacheKey("nas", "a.example.com")]
	res.mu.Unlock()
	if entry.target != "two" {
		t.Fatalf("target = %q, want two", entry.target)
	}
}

func TestCacheKeyIsUnambiguous(t *testing.T) {
	// A name containing a newline must not collide with another pair.
	if cacheKey("a\nb", "c") == cacheKey("a", "b\nc") {
		t.Fatal("cache keys collide")
	}
	if !strings.Contains(fmt.Sprint(cacheKey("nas", "a.example.com")), "nas") {
		t.Fatal("cache key lost the tunnel name")
	}
}

/* ---------------------------------------------- DoH 不可达时的系统解析器回退 */

// failingDoH points the resolver at a DoH endpoint that always fails, the way a
// network that resets connections to cloudflare-dns.com does.
func failingDoH(t *testing.T, res *Resolver) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "reset", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	res.dohEndpoint = srv.URL
}

// TestLookupCNAMEFallsBackToSystemResolver covers a real NAS: DoH to
// cloudflare-dns.com is reset by the network, so every lookup failed and every
// start re-issued `tunnel route dns` for all three hostnames.
func TestLookupCNAMEFallsBackToSystemResolver(t *testing.T) {
	res, _ := newTestResolver(t)
	failingDoH(t, res)
	res.systemLookup = func(hostname string) (string, error) {
		return "ead489d1.cfargotunnel.com.", nil
	}

	got, err := res.LookupCNAME("nas.example.com")
	if err != nil {
		t.Fatalf("DoH 失败时应回退到系统解析器，得到错误: %v", err)
	}
	if got != "ead489d1.cfargotunnel.com" {
		t.Errorf("应返回系统解析器的结果（去掉尾点），得到 %q", got)
	}
}

// TestLookupCNAMEFallbackReportsNoCNAME: the system resolver returning the name
// itself means there is no CNAME, which must trigger a route rather than look
// like a correct one.
func TestLookupCNAMEFallbackReportsNoCNAME(t *testing.T) {
	res, _ := newTestResolver(t)
	failingDoH(t, res)
	res.systemLookup = func(hostname string) (string, error) {
		return hostname + ".", nil
	}

	got, err := res.LookupCNAME("nas.example.com")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != "" {
		t.Errorf("没有 CNAME 时应返回空字符串，得到 %q", got)
	}
}

func TestLookupCNAMEBothResolversFail(t *testing.T) {
	res, _ := newTestResolver(t)
	failingDoH(t, res)
	res.systemLookup = func(string) (string, error) {
		return "", errors.New("no such host")
	}

	_, err := res.LookupCNAME("nas.example.com")
	if err == nil {
		t.Fatal("两个解析器都失败时应返回错误")
	}
	// The DoH message names the real cause; a generic "no such host" would hide it.
	if err.Error() != "DNS 查询失败 (HTTP 502)" {
		t.Errorf("应报告 DoH 的失败原因，得到 %q", err.Error())
	}
}

// TestEnsureRoutesWhenDoHIsBlocked is the end-to-end version: with DoH down the
// check must still reach a decision instead of silently re-routing forever.
func TestEnsureRoutesWhenDoHIsBlocked(t *testing.T) {
	res, h := newTestResolver(t)
	// Ensure() refuses to touch DNS without an account certificate.
	writeCert(t, h)
	failingDoH(t, res)
	res.systemLookup = func(string) (string, error) {
		// Must match what Ensure() expects: <uuid>.cfargotunnel.com.
		return testUUID + ".cfargotunnel.com.", nil
	}
	var routed []string
	res.routeFn = func(name, hostname string) error {
		routed = append(routed, hostname)
		return nil
	}

	out := res.Ensure(localTunnel())
	if !out.OK {
		t.Fatalf("应判定成功，得到 %+v", out)
	}
	if len(routed) != 0 {
		t.Errorf("系统解析器已确认 CNAME 正确，不应再下发路由，实际下发了 %v", routed)
	}
	if len(out.AlreadyOK) != 2 {
		t.Errorf("两个域名都应报告为正确，得到 %+v", out.AlreadyOK)
	}
}
