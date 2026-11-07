package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
)

// assertPrivateMode checks that a file holding secrets is not group/world
// readable.
func assertPrivateMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("文件不存在: %v", err)
	}
	if mode := st.Mode().Perm(); mode != want {
		t.Errorf("%s 权限应为 %o，得到 %o", filepath.Base(path), want, mode)
	}
}

// testLogger returns a Logger that writes nothing to the console, so test output
// stays readable.
func testLogger(t *testing.T, dir string) *applog.Logger {
	t.Helper()
	log := applog.New(filepath.Join(dir, "server.log"))
	log.Out = nil
	log.Err = nil
	t.Cleanup(log.Close)
	return log
}

func decode(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("测试用例的 JSON 非法: %v", err)
	}
	return out
}

/* ---------------------------------------------------------------- 规范化 */

func TestNormalizeTunnelDefaults(t *testing.T) {
	// A tunnel written by an older version has no autoDns field at all. It must
	// come back enabled: without the DNS route the hostname answers a Cloudflare
	// edge error while the tunnel looks healthy.
	got := NormalizeTunnel(rawTunnel{Name: ptr("我的隧道")})

	if got.Type != TypeToken {
		t.Errorf("缺省类型应为 token，得到 %q", got.Type)
	}
	if !got.Enabled || !got.AutoStart || !got.AutoDNS {
		t.Errorf("三个布尔默认都应为 true，得到 enabled=%v autoStart=%v autoDns=%v",
			got.Enabled, got.AutoStart, got.AutoDNS)
	}
	if got.CatchAll != "http_status:404" {
		t.Errorf("catchAll 默认应为 http_status:404，得到 %q", got.CatchAll)
	}
	if !SafeIDRE.MatchString(got.ID) {
		t.Errorf("生成的 id 必须能当路径片段用，得到 %q", got.ID)
	}
	if got.Name != "我的隧道" {
		t.Errorf("名称应保留，得到 %q", got.Name)
	}
}

func TestNormalizeTunnelKeepsExplicitFalse(t *testing.T) {
	got := NormalizeTunnel(rawTunnel{Name: ptr("x"), Enabled: boolPtr(false), AutoDNS: boolPtr(false)})
	if got.Enabled {
		t.Error("显式 enabled=false 必须保留")
	}
	if got.AutoDNS {
		t.Error("显式 autoDns=false 必须保留")
	}
	if !got.AutoStart {
		t.Error("未出现的 autoStart 仍应为 true")
	}
}

func TestNormalizeTunnelReplacesUnsafeID(t *testing.T) {
	// A hand-edited or corrupted config.json is the only way an id like this
	// reaches disk, but it would make config.yml and the log land outside the
	// data directory.
	for _, id := range []string{"../../etc/passwd", "a/b", "", "with space", strings.Repeat("a", 65)} {
		got := NormalizeTunnel(rawTunnel{ID: ptr(id), Name: ptr("x")})
		if got.ID == id {
			t.Errorf("不安全的 id %q 必须被替换", id)
		}
		if !SafeIDRE.MatchString(got.ID) {
			t.Errorf("替换后的 id 仍不安全: %q", got.ID)
		}
	}
}

func TestNormalizeTunnelKeepsSafeID(t *testing.T) {
	got := NormalizeTunnel(rawTunnel{ID: ptr("t_abc-123_XYZ"), Name: ptr("x")})
	if got.ID != "t_abc-123_XYZ" {
		t.Errorf("安全的 id 必须原样保留，得到 %q", got.ID)
	}
}

func TestNormalizeTunnelTruncatesName(t *testing.T) {
	long := strings.Repeat("名", 120)
	got := NormalizeTunnel(rawTunnel{Name: ptr("  " + long + "  ")})
	if n := len([]rune(got.Name)); n != 80 {
		t.Errorf("名称应截断到 80 个字符，得到 %d", n)
	}
	if strings.HasPrefix(got.Name, " ") || strings.HasSuffix(got.Name, " ") {
		t.Errorf("名称两端空白应被去掉，得到 %q", got.Name)
	}
}

func TestNormalizeTunnelUnknownType(t *testing.T) {
	got := NormalizeTunnel(rawTunnel{Name: ptr("x"), Type: ptr("bogus")})
	if got.Type != TypeToken {
		t.Errorf("未知类型应回落到 token，得到 %q", got.Type)
	}
}

/* ------------------------------------------------------------------ 合并 */

func TestMergeTunnelPartialUpdateKeepsOtherFields(t *testing.T) {
	existing := NormalizeTunnel(rawTunnel{
		ID:    ptr("t_keep"),
		Name:  ptr("旧名字"),
		Type:  ptr(TypeToken),
		Token: ptr("secret-token-value"),
		Note:  ptr("备注"),
	})
	merged, err := MergeTunnel(&existing, decode(t, `{"name":"新名字"}`))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if merged.Name != "新名字" {
		t.Errorf("名称应更新，得到 %q", merged.Name)
	}
	if merged.ID != "t_keep" {
		t.Errorf("未提交的 id 必须保留，得到 %q", merged.ID)
	}
	if merged.Token != "secret-token-value" {
		t.Error("未提交的 token 必须保留")
	}
	if merged.Note != "备注" {
		t.Error("未提交的备注必须保留")
	}
}

func TestMergeTunnelTokenRules(t *testing.T) {
	existing := NormalizeTunnel(rawTunnel{
		ID: ptr("t1"), Name: ptr("x"), Type: ptr(TypeToken), Token: ptr("original-token"),
	})

	// The UI never receives the token, so an empty string means "unchanged".
	kept, err := MergeTunnel(&existing, decode(t, `{"token":""}`))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if kept.Token != "original-token" {
		t.Errorf("空字符串应表示不修改，得到 %q", kept.Token)
	}

	// JSON null is the explicit "clear it" signal.
	cleared, err := MergeTunnel(&existing, decode(t, `{"token":null}`))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if cleared.Token != "" {
		t.Errorf("null 应清空 token，得到 %q", cleared.Token)
	}

	// Absent means unchanged as well.
	absent, err := MergeTunnel(&existing, decode(t, `{"name":"y"}`))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if absent.Token != "original-token" {
		t.Errorf("未提交 token 时应保留，得到 %q", absent.Token)
	}
}

func TestMergeTunnelRejectsWrongType(t *testing.T) {
	if _, err := MergeTunnel(nil, decode(t, `{"name":123}`)); err == nil {
		t.Error("类型错误的字段应报错，而不是被静默转换成字符串")
	}
}

func TestMergeTunnelIngressReplacedWholesale(t *testing.T) {
	existing := NormalizeTunnel(rawTunnel{
		ID: ptr("t1"), Name: ptr("x"), Type: ptr(TypeLocal), TunnelName: ptr("nas"),
		Ingress: []Ingress{{Hostname: "old.example.com", Service: "http://127.0.0.1:80"}},
	})
	merged, err := MergeTunnel(&existing, decode(t,
		`{"ingress":[{"hostname":"new.example.com","service":"https://127.0.0.1:443"}]}`))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if len(merged.Ingress) != 1 || merged.Ingress[0].Hostname != "new.example.com" {
		t.Errorf("入站规则应整体替换，得到 %+v", merged.Ingress)
	}
}

/* ------------------------------------------------------------------ 校验 */

func TestValidateTunnelMessages(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		expect string
	}{
		{"缺少名称", `{"type":"token","token":"t"}`, "请填写名称"},
		{"Token 隧道缺 token", `{"name":"a","type":"token"}`, "请填写隧道 Token"},
		{"快速隧道地址非法", `{"name":"a","type":"quick","target":"127.0.0.1:80"}`,
			"本地服务地址需以 http:// 或 https:// 开头"},
		{"本地隧道未选隧道", `{"name":"a","type":"local"}`, "请选择要运行的隧道"},
		{"自定义配置缺路径", `{"name":"a","type":"config"}`, "请填写配置文件路径"},
		{"自定义配置路径穿越", `{"name":"a","type":"config","configFile":"../etc/x.yml"}`, "配置文件路径非法"},
		{"入站规则缺域名", `{"name":"a","type":"token","token":"t","ingress":[{"service":"http://x"}]}`,
			"入站规则缺少域名"},
		{"入站规则服务协议", `{"name":"a","type":"token","token":"t","ingress":[{"hostname":"a.example.com","service":"x"}]}`,
			"入站规则 a.example.com 的服务地址需以 http:// 或 https:// 开头"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := MergeTunnel(nil, decode(t, tc.body))
			if err != nil {
				t.Fatalf("合并失败: %v", err)
			}
			problems := ValidateTunnel(merged)
			joined := strings.Join(problems, "；")
			if !strings.Contains(joined, tc.expect) {
				t.Errorf("期望包含 %q，实际是 %q", tc.expect, joined)
			}
		})
	}
}

func TestValidateTunnelHostnameMessage(t *testing.T) {
	merged, err := MergeTunnel(nil, decode(t,
		`{"name":"a","type":"token","token":"t","ingress":[{"hostname":"https://a.example.com/x","service":"http://x"}]}`))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	joined := strings.Join(ValidateTunnel(merged), "；")
	want := "入站规则域名不合法：https://a.example.com/x（只填主机名，例如 nas.example.com，不要带协议、端口或路径）"
	if !strings.Contains(joined, want) {
		t.Errorf("期望包含 %q，实际是 %q", want, joined)
	}
}

func TestValidateTunnelAcceptsGoodInput(t *testing.T) {
	merged, err := MergeTunnel(nil, decode(t, `{
		"name":"家庭 NAS","type":"local","tunnelName":"nas",
		"ingress":[{"hostname":"nas.example.com","service":"http://127.0.0.1:5666"}]}`))
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if problems := ValidateTunnel(merged); len(problems) > 0 {
		t.Errorf("合法输入不应报错，得到 %v", problems)
	}
}

/* ------------------------------------------------------------- 掩码与 id */

func TestMaskSecret(t *testing.T) {
	if MaskSecret("") != "" {
		t.Error("空值应返回空")
	}
	if got := MaskSecret("short"); got != "••••••••" {
		t.Errorf("短值应整体掩码，得到 %q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz"
	got := MaskSecret(long)
	if strings.Contains(got, long) {
		t.Error("掩码不得包含完整密文")
	}
	if !strings.HasPrefix(got, "abcdef") || !strings.HasSuffix(got, "wxyz") {
		t.Errorf("掩码格式应为前 6 + … + 后 4，得到 %q", got)
	}
}

func TestNewIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if !SafeIDRE.MatchString(id) {
			t.Fatalf("id 不安全: %q", id)
		}
		if !strings.HasPrefix(id, "t_") {
			t.Fatalf("id 应以 t_ 开头: %q", id)
		}
		if seen[id] {
			t.Fatalf("id 重复: %q", id)
		}
		seen[id] = true
	}
}

/* ------------------------------------------------------------------ 存储 */

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	log := testLogger(t, dir)
	path := filepath.Join(dir, "config.json")
	store := NewStore(path, log)
	store.Load()
	return store, path
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	store, path := newStore(t)

	tunnel := NormalizeTunnel(rawTunnel{
		ID: ptr("t_round"), Name: ptr("往返"), Type: ptr(TypeToken), Token: ptr("tok"),
	})
	store.Add(tunnel)
	store.SetSettings(Settings{Protocol: "http2", LogLevel: "debug", MetricsPort: 9000,
		AutoStart: false, AutoRestart: false})
	if err := store.Save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// The file must not be world-readable: it holds tunnel tokens.
	assertPrivateMode(t, path, 0o600)

	reloaded := NewStore(path, testLogger(t, t.TempDir()))
	reloaded.Load()
	if got := reloaded.Settings(); got.Protocol != "http2" || got.LogLevel != "debug" || got.MetricsPort != 9000 {
		t.Errorf("设置未往返: %+v", got)
	}
	if got := reloaded.Settings(); got.AutoStart || got.AutoRestart {
		t.Error("布尔设置未往返")
	}
	tunnels := reloaded.Tunnels()
	if len(tunnels) != 1 || tunnels[0].ID != "t_round" || tunnels[0].Token != "tok" {
		t.Errorf("隧道未往返: %+v", tunnels)
	}
}

func TestStoreCorruptConfigFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path, testLogger(t, dir))
	store.Load() // must not panic
	if got := store.Settings().LogLevel; got != "info" {
		t.Errorf("损坏的配置应回落到默认值，得到 logLevel=%q", got)
	}
	if len(store.Tunnels()) != 0 {
		t.Error("损坏的配置不应产出隧道")
	}
}

// TestStoreSettingsFromAnotherWriterSurvive is the regression for the defect
// where editing any tunnel silently reverted a setting changed on the App Center
// settings page (which runs cmd/config_callback, a separate process).
func TestStoreSettingsFromAnotherWriterSurvive(t *testing.T) {
	store, path := newStore(t)
	store.Add(NormalizeTunnel(rawTunnel{ID: ptr("t_a"), Name: ptr("A")}))
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	// Simulate config_callback writing debug log level behind our back.
	if err := ApplyWizardSettings(path, func(key string) string {
		if key == "wizard_log_level" {
			return "debug"
		}
		return ""
	}); err != nil {
		t.Fatalf("外部写入失败: %v", err)
	}

	// This process now edits a tunnel and saves.
	store.Add(NormalizeTunnel(rawTunnel{ID: ptr("t_b"), Name: ptr("B")}))
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded := NewStore(path, testLogger(t, t.TempDir()))
	reloaded.Load()
	if got := reloaded.Settings().LogLevel; got != "debug" {
		t.Errorf("外部写入的 logLevel 被回滚了，得到 %q（应为 debug）", got)
	}
	if len(reloaded.Tunnels()) != 2 {
		t.Errorf("本进程的隧道写入丢失，得到 %d 条", len(reloaded.Tunnels()))
	}
}

func TestStoreSettingsWrittenHereWin(t *testing.T) {
	store, path := newStore(t)
	store.Add(NormalizeTunnel(rawTunnel{ID: ptr("t_a"), Name: ptr("A")}))
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	// Someone else changes it, then this process changes it too: the local
	// write is the newer one and must win.
	if err := ApplyWizardSettings(path, func(key string) string {
		if key == "wizard_log_level" {
			return "debug"
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	next := store.Settings()
	next.LogLevel = "warn"
	store.SetSettings(next)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded := NewStore(path, testLogger(t, t.TempDir()))
	reloaded.Load()
	if got := reloaded.Settings().LogLevel; got != "warn" {
		t.Errorf("本进程的设置应优先，得到 %q", got)
	}
}

func TestStoreTunnelAccessors(t *testing.T) {
	store, _ := newStore(t)
	store.Add(NormalizeTunnel(rawTunnel{ID: ptr("t_1"), Name: ptr("一")}))

	if _, ok := store.Tunnel("t_1"); !ok {
		t.Fatal("应能取到已添加的隧道")
	}
	if _, ok := store.Tunnel("nope"); ok {
		t.Error("不存在的 id 不应命中")
	}

	replaced := NormalizeTunnel(rawTunnel{ID: ptr("t_1"), Name: ptr("改名")})
	if !store.Replace(replaced) {
		t.Error("Replace 应报告成功")
	}
	if got, _ := store.Tunnel("t_1"); got.Name != "改名" {
		t.Errorf("替换未生效: %q", got.Name)
	}
	if store.Replace(NormalizeTunnel(rawTunnel{ID: ptr("t_missing"), Name: ptr("x")})) {
		t.Error("替换不存在的隧道应报告失败")
	}

	if !store.Update("t_1", func(t *Tunnel) { t.TunnelID = "uuid-1" }) {
		t.Error("Update 应报告成功")
	}
	if got, _ := store.Tunnel("t_1"); got.TunnelID != "uuid-1" {
		t.Error("Update 未生效")
	}

	if !store.Delete("t_1") {
		t.Error("Delete 应报告成功")
	}
	if store.Delete("t_1") {
		t.Error("重复删除应报告失败")
	}
}

// TestStoreReturnsCopies guards the state against a caller that mutates what it
// was handed.
func TestStoreReturnsCopies(t *testing.T) {
	store, _ := newStore(t)
	store.Add(NormalizeTunnel(rawTunnel{
		ID: ptr("t_1"), Name: ptr("一"), Type: ptr(TypeLocal),
		Ingress: []Ingress{{Hostname: "a.example.com", Service: "http://x"}},
	}))

	got, _ := store.Tunnel("t_1")
	got.Name = "被外部改了"
	got.Ingress[0].Hostname = "evil.example.com"

	again, _ := store.Tunnel("t_1")
	if again.Name != "一" {
		t.Error("外部修改不应影响存储内的值")
	}
	if again.Ingress[0].Hostname != "a.example.com" {
		t.Error("外部修改入站规则不应影响存储内的值")
	}

	list := store.Tunnels()
	list[0].Name = "也改了"
	if final, _ := store.Tunnel("t_1"); final.Name != "一" {
		t.Error("Tunnels() 返回的切片不应指向内部状态")
	}
}

/* ------------------------------------------------------------------ 向导 */

func TestApplyWizardSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Pre-existing state, including a key this version does not know about.
	seed := `{
  "version": 1,
  "futureKey": {"keep": true},
  "settings": {"protocol": "auto", "logLevel": "info", "metricsPort": 1234},
  "tunnels": [{"id": "t_keep", "name": "保留", "type": "token", "token": "secret"}]
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		"wizard_auto_start":   "false",
		"wizard_auto_restart": "1",
		"wizard_log_level":    "debug",
		"wizard_protocol":     "quic",
	}
	if err := ApplyWizardSettings(path, func(k string) string { return env[k] }); err != nil {
		t.Fatalf("应用向导设置失败: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("写出的文件不是合法 JSON: %v", err)
	}

	settings := root["settings"].(map[string]any)
	if settings["autoStart"] != false {
		t.Errorf("autoStart 应为 false，得到 %v", settings["autoStart"])
	}
	if settings["autoRestart"] != true {
		t.Errorf("autoRestart 应为 true，得到 %v", settings["autoRestart"])
	}
	if settings["logLevel"] != "debug" || settings["protocol"] != "quic" {
		t.Errorf("枚举设置未生效: %v", settings)
	}
	if settings["metricsPort"] != float64(1234) {
		t.Errorf("未提交的设置应保留，得到 %v", settings["metricsPort"])
	}
	if _, ok := root["futureKey"]; !ok {
		t.Error("未知的顶层键必须保留")
	}
	tunnels, ok := root["tunnels"].([]any)
	if !ok || len(tunnels) != 1 {
		t.Fatalf("隧道定义必须原样保留，得到 %v", root["tunnels"])
	}
	if tunnels[0].(map[string]any)["token"] != "secret" {
		t.Error("隧道 Token 必须原样保留")
	}

	assertPrivateMode(t, path, 0o600)
}

func TestApplyWizardSettingsIgnoresInvalidValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"settings":{"protocol":"auto","logLevel":"info"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"wizard_protocol":   "carrier-pigeon", // 非法枚举：必须被忽略
		"wizard_log_level":  "",
		"wizard_auto_start": "maybe", // 非法布尔：必须被忽略
	}
	if err := ApplyWizardSettings(path, func(k string) string { return env[k] }); err != nil {
		t.Fatalf("应用向导设置失败: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var root map[string]any
	_ = json.Unmarshal(raw, &root)
	settings := root["settings"].(map[string]any)
	if settings["protocol"] != "auto" {
		t.Errorf("非法协议值应被忽略，得到 %v", settings["protocol"])
	}
	if settings["logLevel"] != "info" {
		t.Errorf("空值应被忽略，得到 %v", settings["logLevel"])
	}
	if _, exists := settings["autoStart"]; exists {
		t.Errorf("无法识别的布尔值应被忽略，得到 %v", settings["autoStart"])
	}
}

func TestApplyWizardSettingsCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ApplyWizardSettings(path, func(k string) string {
		if k == "wizard_auto_start" {
			return "true"
		}
		return ""
	}); err != nil {
		t.Fatalf("文件不存在时应创建: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("应创建配置文件: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("写出的文件不是合法 JSON: %v", err)
	}
	if root["version"] != float64(1) {
		t.Errorf("应写入 version=1，得到 %v", root["version"])
	}
}

func ptr(s string) *string { return &s }
func boolPtr(b bool) *bool { return &b }
