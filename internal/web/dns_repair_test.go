package web

import (
	"net/http"
	"testing"
)

// A failed manual DNS repair has to explain itself. The frontend's request
// helper reports data.error for any non-2xx response and otherwise falls back
// to "请求失败 (400)"; dnsroute.Result keeps its reason in "message", so before
// this every failure -- not logged in, tunnel UUID unknown, an API rejection --
// reached the user as that generic line and the reason was thrown away.
func TestFailedDNSRepairCarriesErrorField(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, http.MethodPost, "/api/tunnels",
		`{"name":"local1","type":"local","tunnelName":"nas",`+
			`"ingress":[{"hostname":"nas.example.com","service":"http://127.0.0.1:5000"}],`+
			`"autoDns":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建隧道失败: %d %s", rec.Code, rec.Body.String())
	}
	created, _ := decodeJSON(t, rec)["tunnel"].(map[string]any)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("响应里没有隧道 id: %s", rec.Body.String())
	}

	// The fixture has no cert.pem, so Ensure refuses to touch DNS and says why.
	rec = f.do(t, http.MethodPost, "/api/tunnels/"+id+"/dns", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DNS 修复失败时应返回 400，得到 %d %s", rec.Code, rec.Body.String())
	}

	body := decodeJSON(t, rec)
	msg, _ := body["message"].(string)
	if msg == "" {
		t.Fatalf("响应缺少 message: %s", rec.Body.String())
	}
	if got, _ := body["error"].(string); got != msg {
		t.Fatalf("error = %q，期望与 message 相同 (%q)", got, msg)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatalf("ok 应为 false: %s", rec.Body.String())
	}
}

// A successful repair must keep the shape the UI renders (routed/alreadyOk/…)
// and must not grow an "error" key that would look like a failure.
func TestSuccessfulDNSRepairHasNoErrorField(t *testing.T) {
	f := newFixture(t)
	// A token tunnel never gets a repair, so the endpoint answers with the
	// "not applicable" result -- which is still a 200 and must not carry error.
	rec := f.do(t, http.MethodPost, "/api/tunnels", `{"name":"tok","type":"token","token":"eyJhIjoieCJ9"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建隧道失败: %d %s", rec.Code, rec.Body.String())
	}
	created, _ := decodeJSON(t, rec)["tunnel"].(map[string]any)
	id, _ := created["id"].(string)

	rec = f.do(t, http.MethodPost, "/api/tunnels/"+id+"/dns", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Token 隧道的 DNS 修复应返回 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if _, exists := body["error"]; exists {
		t.Fatalf("成功的响应不应包含 error: %s", rec.Body.String())
	}
	for _, key := range []string{"ok", "routed", "alreadyOk", "failed", "skipped", "message"} {
		if _, exists := body[key]; !exists {
			t.Fatalf("成功响应缺少 %q: %s", key, rec.Body.String())
		}
	}
}
