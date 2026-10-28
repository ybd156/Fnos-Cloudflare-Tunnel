package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/cf"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
	"github.com/ybd156/fnos-cloudflared/internal/runner"
)

// fixture is a fully wired service rooted in a temp directory.
type fixture struct {
	server *Server
	paths  paths.Paths
	store  *config.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	p := paths.Rooted(root)

	log := applog.New(p.AppLog)
	log.Out = nil
	log.Err = nil
	t.Cleanup(log.Close)

	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("创建数据目录失败: %v", err)
	}
	// A minimal payload so the static handler has something to serve.
	if err := os.MkdirAll(p.WWW, 0o755); err != nil {
		t.Fatal(err)
	}
	index := `<!doctype html><html><body><script type="module" src="%%CF_BASE%%/js/app.js"></script></body></html>`
	if err := os.WriteFile(filepath.Join(p.WWW, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.WWW, "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.WWW, "js", "app.js"), []byte("export const x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := config.NewStore(p.ConfigFile, log)
	store.Load()

	cfRunner := cf.New(p, log)
	dns := dnsroute.New(p, log, cfRunner)
	manager := runner.New(p, log, store, cfRunner, dns)
	t.Cleanup(manager.StopAll)

	return &fixture{server: New(p, log, store, cfRunner, dns, manager), paths: p, store: store}
}

// do issues a request against the handler directly.
func (f *fixture) do(t *testing.T, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %q", rec.Body.String())
	}
	return out
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	msg, _ := decodeJSON(t, rec)["error"].(string)
	return msg
}

/* --------------------------------------------------------------- 基础路由 */

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz 应返回 200 ok，得到 %d %q", rec.Code, rec.Body.String())
	}
}

func TestMetaReportsRuntimeNotNode(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/meta", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	body := decodeJSON(t, rec)
	if body["runtime"] == nil || body["runtime"] == "" {
		t.Error("应报告 Go 运行时版本")
	}
	if _, exists := body["node"]; exists {
		t.Error("服务已不是 Node 程序，不应再返回 node 字段")
	}
	if body["app"] != "cloudflared" {
		t.Errorf("app 字段不符: %v", body["app"])
	}
	if body["binaryExists"] != false {
		t.Error("测试环境没有 cloudflared 二进制，binaryExists 应为 false")
	}
	if body["gatewayPrefix"] != "/app/cloudflared" {
		t.Errorf("gatewayPrefix 不符: %v", body["gatewayPrefix"])
	}
}

func TestUnknownAPIResource(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("期望 404，得到 %d", rec.Code)
	}
	if msg := errorOf(t, rec); msg != "接口不存在" {
		t.Errorf("错误文案不符: %q", msg)
	}
}

/* ----------------------------------------------------------------- 隧道 */

func TestListTunnelsEmpty(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/tunnels", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	tunnels, ok := decodeJSON(t, rec)["tunnels"].([]any)
	if !ok {
		t.Fatalf("应返回 tunnels 数组: %q", rec.Body.String())
	}
	if len(tunnels) != 0 {
		t.Errorf("初始应为空，得到 %d 条", len(tunnels))
	}
}

func TestCreateTunnelRequiresName(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/api/tunnels", `{"type":"token","token":"t"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d", rec.Code)
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "请填写名称") {
		t.Errorf("错误文案不符: %q", msg)
	}
}

func TestCreateTunnelPersistsAndMasksToken(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/api/tunnels",
		`{"name":"生产隧道","type":"token","token":"super-secret-token-value"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	tunnel, ok := body["tunnel"].(map[string]any)
	if !ok {
		t.Fatalf("应返回 tunnel: %q", rec.Body.String())
	}
	if tunnel["hasToken"] != true {
		t.Error("应标记 hasToken")
	}
	// The token itself must never reach the browser.
	if strings.Contains(rec.Body.String(), "super-secret-token-value") {
		t.Error("响应里出现了完整 Token")
	}
	if preview, _ := tunnel["tokenPreview"].(string); preview == "" || preview == "super-secret-token-value" {
		t.Errorf("tokenPreview 应为掩码，得到 %q", preview)
	}

	// It must be on disk.
	reloaded := config.NewStore(f.paths.ConfigFile, f.storeLogger(t))
	reloaded.Load()
	tunnels := reloaded.Tunnels()
	if len(tunnels) != 1 || tunnels[0].Name != "生产隧道" {
		t.Fatalf("隧道未落盘: %+v", tunnels)
	}
	if tunnels[0].Token != "super-secret-token-value" {
		t.Error("Token 应保存在服务端")
	}
}

// storeLogger builds a throwaway logger for reloading the config in a test.
func (f *fixture) storeLogger(t *testing.T) *applog.Logger {
	t.Helper()
	log := applog.New(f.paths.AppLog)
	log.Out = nil
	log.Err = nil
	t.Cleanup(log.Close)
	return log
}

func TestCreateTunnelValidationMessages(t *testing.T) {
	cases := []struct {
		body   string
		expect string
	}{
		{`{"name":"a","type":"quick","target":"127.0.0.1:80"}`, "本地服务地址需以 http:// 或 https:// 开头"},
		{`{"name":"a","type":"token"}`, "请填写隧道 Token"},
		{`{"name":"a","type":"local"}`, "请选择要运行的隧道"},
	}
	for _, tc := range cases {
		f := newFixture(t)
		rec := f.do(t, http.MethodPost, "/api/tunnels", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: 期望 400，得到 %d", tc.body, rec.Code)
			continue
		}
		if msg := errorOf(t, rec); !strings.Contains(msg, tc.expect) {
			t.Errorf("%s: 期望包含 %q，得到 %q", tc.body, tc.expect, msg)
		}
	}
}

func TestGetUpdateDeleteTunnel(t *testing.T) {
	f := newFixture(t)
	created := decodeJSON(t, f.do(t, http.MethodPost, "/api/tunnels",
		`{"name":"原名","type":"token","token":"tok"}`))["tunnel"].(map[string]any)
	id := created["id"].(string)

	// GET
	rec := f.do(t, http.MethodGet, "/api/tunnels/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 期望 200，得到 %d", rec.Code)
	}

	// PUT keeps the id and the token (the UI cannot echo it back).
	rec = f.do(t, http.MethodPut, "/api/tunnels/"+id, `{"name":"改名","token":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 期望 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	updated := decodeJSON(t, rec)["tunnel"].(map[string]any)
	if updated["name"] != "改名" {
		t.Errorf("名称未更新: %v", updated["name"])
	}
	if updated["id"] != id {
		t.Errorf("id 不应变化: %v", updated["id"])
	}
	if updated["hasToken"] != true {
		t.Error("空 token 表示不修改，hasToken 应仍为 true")
	}

	// DELETE
	rec = f.do(t, http.MethodDelete, "/api/tunnels/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE 期望 200，得到 %d", rec.Code)
	}
	if rec = f.do(t, http.MethodGet, "/api/tunnels/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("删除后 GET 应 404，得到 %d", rec.Code)
	}
	reloaded := config.NewStore(f.paths.ConfigFile, f.storeLogger(t))
	reloaded.Load()
	if len(reloaded.Tunnels()) != 0 {
		t.Error("删除后配置里不应还有该隧道")
	}
}

// TestUnknownTunnelIDRoutes404 is the regression for the endpoint that created a
// permanent supervisor entry for any id in the URL. The observable contract is
// that every :id route answers 404 for a tunnel that does not exist.
func TestUnknownTunnelIDRoutes404(t *testing.T) {
	f := newFixture(t)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/tunnels/ghost"},
		{http.MethodGet, "/api/tunnels/ghost/logs"},
		{http.MethodPost, "/api/tunnels/ghost/stop"},
		{http.MethodPost, "/api/tunnels/ghost/restart"},
		{http.MethodPost, "/api/tunnels/ghost/dns"},
		{http.MethodPut, "/api/tunnels/ghost"},
		{http.MethodDelete, "/api/tunnels/ghost"},
	}
	for _, route := range routes {
		rec := f.do(t, route.method, route.path, `{"name":"x"}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s 期望 404，得到 %d", route.method, route.path, rec.Code)
		}
	}
	// Hammering unknown ids must not disturb real state.
	for i := 0; i < 50; i++ {
		f.do(t, http.MethodGet, "/api/tunnels/ghost"+string(rune('a'+i%26))+"/logs", "")
	}
	reloaded := config.NewStore(f.paths.ConfigFile, f.storeLogger(t))
	reloaded.Load()
	if len(reloaded.Tunnels()) != 0 {
		t.Error("未知 id 的请求不应改动配置")
	}
}

func TestTunnelLogsEndpoint(t *testing.T) {
	f := newFixture(t)
	created := decodeJSON(t, f.do(t, http.MethodPost, "/api/tunnels",
		`{"name":"日志","type":"token","token":"tok"}`))["tunnel"].(map[string]any)
	id := created["id"].(string)

	// Seed a log file: the endpoint falls back to the bounded tail read when the
	// in-memory ring buffer is empty.
	content := strings.Repeat("line\n", 10)
	if err := os.WriteFile(f.paths.TunnelLog(id), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, http.MethodGet, "/api/tunnels/"+id+"/logs?lines=3", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	lines, ok := decodeJSON(t, rec)["lines"].([]any)
	if !ok {
		t.Fatalf("应返回 lines 数组: %q", rec.Body.String())
	}
	if len(lines) != 3 {
		t.Errorf("应返回 3 行，得到 %d", len(lines))
	}
}

/* ----------------------------------------------------------------- 设置 */

func TestSettingsGetAndPut(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	if decodeJSON(t, rec)["protocol"] != "auto" {
		t.Errorf("默认协议应为 auto: %q", rec.Body.String())
	}

	rec = f.do(t, http.MethodPut, "/api/settings", `{"protocol":"http2","logLevel":"debug","metricsPort":9000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 期望 200，得到 %d: %s", rec.Code, rec.Body.String())
	}

	reloaded := config.NewStore(f.paths.ConfigFile, f.storeLogger(t))
	reloaded.Load()
	got := reloaded.Settings()
	if got.Protocol != "http2" || got.LogLevel != "debug" || got.MetricsPort != 9000 {
		t.Errorf("设置未落盘: %+v", got)
	}
}

func TestSettingsRejectsInvalidValues(t *testing.T) {
	cases := []struct{ body, expect string }{
		{`{"protocol":"carrier-pigeon"}`, "protocol 取值非法"},
		{`{"edgeIpVersion":"5"}`, "edgeIpVersion 取值非法"},
		{`{"logLevel":"verbose"}`, "logLevel 取值非法"},
		{`{"metricsPort":70000}`, "metrics 端口超出范围"},
	}
	for _, tc := range cases {
		f := newFixture(t)
		rec := f.do(t, http.MethodPut, "/api/settings", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: 期望 400，得到 %d", tc.body, rec.Code)
			continue
		}
		if msg := errorOf(t, rec); msg != tc.expect {
			t.Errorf("%s: 期望 %q，得到 %q", tc.body, tc.expect, msg)
		}
	}
}

func TestSettingsAcceptsNumericEdgeIPVersion(t *testing.T) {
	// The old API tolerated 4 and "4"; a form control can send either.
	f := newFixture(t)
	rec := f.do(t, http.MethodPut, "/api/settings", `{"edgeIpVersion":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := decodeJSON(t, rec)["settings"].(map[string]any)["edgeIpVersion"]; got != "4" {
		t.Errorf("数字 4 应归一化为字符串 \"4\"，得到 %v", got)
	}
}

func TestSettingsMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodDelete, "/api/settings", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("期望 405，得到 %d", rec.Code)
	}
	if msg := errorOf(t, rec); msg != "方法不允许" {
		t.Errorf("错误文案不符: %q", msg)
	}
}

/* --------------------------------------------------------------- 请求体 */

func TestBodyNotJSON(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/api/tunnels", "not json at all")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，得到 %d", rec.Code)
	}
	if msg := errorOf(t, rec); msg != "请求体不是合法 JSON" {
		t.Errorf("错误文案不符: %q", msg)
	}
}

func TestBodyTooLarge(t *testing.T) {
	f := newFixture(t)
	huge := `{"name":"` + strings.Repeat("x", paths.MaxBodyBytes) + `"}`
	rec := f.do(t, http.MethodPost, "/api/tunnels", huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("期望 413，得到 %d", rec.Code)
	}
	if msg := errorOf(t, rec); msg != "请求体过大" {
		t.Errorf("错误文案不符: %q", msg)
	}
}

/* --------------------------------------------------------------- 静态资源 */

func TestStaticServesIndexAtRoot(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/js/app.js") {
		t.Errorf("首页内容不符: %q", body)
	}
	// Without the gateway prefix the placeholder must collapse to nothing.
	if strings.Contains(body, "%%CF_BASE%%") {
		t.Error("占位符未被替换")
	}
}

func TestStaticGatewayPrefixIsInjected(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/app/cloudflared/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/app/cloudflared/js/app.js") {
		t.Errorf("网关前缀未被注入: %q", rec.Body.String())
	}
}

func TestStaticGatewayPrefixedAPI(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/app/cloudflared/api/tunnels", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("带网关前缀的 API 应可用，得到 %d", rec.Code)
	}
}

func TestStaticSpaFallback(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/tunnels/deep/link", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("无扩展名的未知路径应回落到 index.html，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Error("应返回 index.html")
	}
}

func TestStaticMissingAsset404(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/js/missing.js", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("缺失的静态资源应 404，得到 %d", rec.Code)
	}
}

// TestStaticPathTraversalRejected covers the defect where a plain prefix
// comparison accepted a sibling directory (…/www-evil) as being inside www.
func TestStaticPathTraversalRejected(t *testing.T) {
	f := newFixture(t)
	root := filepath.Dir(f.paths.WWW)
	// A file next to www, and a sibling directory whose name shares the prefix.
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	evil := f.paths.WWW + "-evil"
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, "x.txt"), []byte("EVIL"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		"/../secret.txt",
		"/../../secret.txt",
		"/%2e%2e/secret.txt",
		"/../www-evil/x.txt",
	} {
		rec := f.do(t, http.MethodGet, target, "")
		body := rec.Body.String()
		if strings.Contains(body, "TOP-SECRET") || strings.Contains(body, "EVIL") {
			t.Errorf("%s 读到了 www 之外的文件", target)
		}
		if rec.Code == http.StatusOK && !strings.Contains(body, "<!doctype html>") {
			t.Errorf("%s 返回了非首页内容: %q", target, body)
		}
	}
}

func TestStaticOnlyAllowsGetAndHead(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/index.html", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("静态资源应只接受 GET/HEAD，得到 %d", rec.Code)
	}
}

// TestMalformedPercentEncodingDoesNotCrash drives a raw request line at the real
// server: a malformed escape used to throw out of the handler and kill the whole
// process, orphaning every running tunnel.
func TestMalformedPercentEncodingDoesNotCrash(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(f.server.Handler())
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接测试服务器失败: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("GET /% HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatalf("写入请求失败: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("读取响应失败（服务可能已崩溃）: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("畸形百分号编码应返回 4xx，得到 %d", resp.StatusCode)
	}

	// The service must still be serving afterwards.
	rec := f.do(t, http.MethodGet, "/api/tunnels", "")
	if rec.Code != http.StatusOK {
		t.Errorf("畸形请求之后服务应仍可用，得到 %d", rec.Code)
	}
}

/* ----------------------------------------------------------------- 账户 */

func TestAccountStatusWhenLoggedOut(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/account", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	body := decodeJSON(t, rec)
	if body["loggedIn"] != false {
		t.Error("未登录时应报告 loggedIn=false")
	}
	if _, ok := body["certPath"]; !ok {
		t.Error("应返回 certPath")
	}
}

// TestAccountLoginWithoutBinaryReportsErrorViaStatus pins the contract inherited
// from the previous implementation: the login request is accepted immediately
// and the outcome arrives through the polled status, because the child process
// error is asynchronous.
func TestAccountLoginWithoutBinaryReportsErrorViaStatus(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/api/account/login", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200（与旧实现一致），得到 %d: %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	var status map[string]any
	for time.Now().Before(deadline) {
		status = decodeJSON(t, f.do(t, http.MethodGet, "/api/account/login", ""))
		if status["status"] == "error" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status["status"] != "error" {
		t.Errorf("二进制缺失时登录状态应变为 error，得到 %v", status["status"])
	}
	if msg, _ := status["error"].(string); msg == "" {
		t.Error("应给出错误信息")
	}
	if status["loggedIn"] != false {
		t.Error("没有 cert.pem 时不应报告已登录")
	}
}

func TestAccountLogout(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/api/account/logout", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
}

/* --------------------------------------------------------------- 应用日志 */

func TestAppLogEndpoint(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.paths.AppLog, []byte("first\nsecond\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, http.MethodGet, "/api/logs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	lines, ok := decodeJSON(t, rec)["lines"].([]any)
	if !ok || len(lines) != 2 {
		t.Errorf("应返回 2 行，得到 %q", rec.Body.String())
	}
}

func TestAppLogEndpointEmptyIsArray(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodGet, "/api/logs", "")
	// A nil slice would marshal as null, which the UI would have to special-case.
	if !strings.Contains(rec.Body.String(), `"lines":[]`) {
		t.Errorf("空日志应返回空数组而不是 null: %q", rec.Body.String())
	}
}
