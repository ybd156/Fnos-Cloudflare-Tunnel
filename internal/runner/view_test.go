package runner

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/config"
)

// nodeViewKeys is the exact key set of the previous implementation's
// tunnelView object. The front end reads these names, so a rename is a
// breaking change even though the compiler would not notice.
var nodeViewKeys = []string{
	"id", "name", "type", "enabled", "autoStart", "autoDns", "target",
	"quickHostname", "tunnelName", "tunnelId", "credentialsFile", "configFile",
	"ingress", "catchAll", "extraArgs", "note", "hasToken", "tokenPreview",
	"status", "pid", "url", "error", "startedAt", "stoppedAt", "lastExitCode",
	"lastExitAt", "attempts", "dnsStatus", "dnsResults", "dnsMessage",
	"dnsRunning",
	// Added in 1.4.2. Deliberate additions, not renames: origin health is a
	// separate concern from the tunnel's own status, because a tunnel can be
	// connected at the edge while every forwarded request fails.
	"originError", "origins",
}

func TestViewJSONMatchesLegacyShape(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	tunnel, ok := h.store.Tunnel("a")
	if !ok {
		t.Fatal("tunnel a missing from the store")
	}

	raw, err := json.Marshal(h.m.View(tunnel))
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}

	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := append([]string(nil), nodeViewKeys...)
	sort.Strings(want)

	if len(keys) != len(want) {
		t.Fatalf("view has %d keys, want %d\n got: %v\nwant: %v", len(keys), len(want), keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("key %d = %q, want %q (full: %v)", i, keys[i], want[i], keys)
		}
	}
}

func TestViewDefaultsForNeverStartedTunnel(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	tunnel, _ := h.store.Tunnel("a")

	view := h.m.View(tunnel)
	if view.Status != StatusStopped {
		t.Fatalf("status = %q, want %q", view.Status, StatusStopped)
	}
	if view.PID != nil {
		t.Fatalf("pid = %v, want null", *view.PID)
	}
	if view.StartedAt != nil || view.StoppedAt != nil || view.LastExitAt != nil {
		t.Fatal("timestamps of a tunnel that never started must be null")
	}
	if view.LastExitCode != nil {
		t.Fatal("lastExitCode must be null before the first exit")
	}
	if view.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0", view.Attempts)
	}
	if view.DNSResults != nil {
		t.Fatal("dnsResults must be null before the first check")
	}
	if view.DNSStatus != "" || view.DNSMessage != "" || view.DNSRunning {
		t.Fatalf("dns fields should be empty, got %q/%q/%v", view.DNSStatus, view.DNSMessage, view.DNSRunning)
	}
	if view.URL != "" || view.Error != "" {
		t.Fatalf("url/error should be empty, got %q/%q", view.URL, view.Error)
	}

	// The nullable fields must really encode as JSON null, which is what the
	// front end checks for.
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"pid":null`, `"startedAt":null`, `"stoppedAt":null`, `"lastExitAt":null`, `"lastExitCode":null`, `"dnsResults":null`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("view JSON missing %s: %s", field, raw)
		}
	}
}

func TestViewCopiesTunnelDefinition(t *testing.T) {
	tunnel := testTunnel("a")
	tunnel.Name = "我的隧道"
	tunnel.Type = config.TypeToken
	tunnel.Token = "eyJhIjoiYWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoifQ=="
	tunnel.Note = "note"
	tunnel.CatchAll = "http_status:404"
	tunnel.ExtraArgs = "--edge-ip-version 4"
	tunnel.QuickHostname = "quick.example.com"
	tunnel.TunnelID = "11111111-2222-3333-4444-555555555555"
	tunnel.Ingress = []config.Ingress{{
		Hostname: "nas.example.com",
		Service:  "http://127.0.0.1:5666",
	}}

	h := newHarness(t, tunnel)
	stored, _ := h.store.Tunnel("a")
	view := h.m.View(stored)

	if view.ID != "a" || view.Name != "我的隧道" || view.Type != config.TypeToken {
		t.Fatalf("identity fields wrong: %+v", view)
	}
	if view.Note != "note" || view.CatchAll != "http_status:404" || view.ExtraArgs != "--edge-ip-version 4" {
		t.Fatalf("definition fields wrong: %+v", view)
	}
	if view.QuickHostname != "quick.example.com" || view.TunnelID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("tunnel identity fields wrong: %+v", view)
	}
	if len(view.Ingress) != 1 || view.Ingress[0].Hostname != "nas.example.com" {
		t.Fatalf("ingress not carried over: %+v", view.Ingress)
	}
}

func TestViewNeverExposesTheToken(t *testing.T) {
	tunnel := testTunnel("a")
	tunnel.Type = config.TypeToken
	secret := "eyJhIjoiYWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoifQ=="
	tunnel.Token = secret

	h := newHarness(t, tunnel)
	stored, _ := h.store.Tunnel("a")
	view := h.m.View(stored)

	if !view.HasToken {
		t.Fatal("hasToken must be true when a token is stored")
	}
	if view.TokenPreview != config.MaskSecret(secret) {
		t.Fatalf("tokenPreview = %q, want %q", view.TokenPreview, config.MaskSecret(secret))
	}

	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the view leaks the tunnel token: %s", raw)
	}
	// Look for an actual field, not a substring: the tunnel *type* is "token".
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fields["token"]; ok {
		t.Fatalf("the view must not carry the token itself: %s", raw)
	}
}

func TestViewWithoutToken(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	stored, _ := h.store.Tunnel("a")
	view := h.m.View(stored)

	if view.HasToken {
		t.Fatal("hasToken must be false without a token")
	}
	if view.TokenPreview != "" {
		t.Fatalf("tokenPreview = %q, want empty", view.TokenPreview)
	}
}

func TestViewIngressIsAlwaysAnArray(t *testing.T) {
	tunnel := testTunnel("a")
	tunnel.Ingress = nil

	h := newHarness(t, tunnel)
	stored, _ := h.store.Tunnel("a")
	raw, err := json.Marshal(h.m.View(stored))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"ingress":[]`) {
		t.Fatalf("empty ingress must encode as [], got: %s", raw)
	}
}

func TestViewReportsRuntimeRecord(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	tunnel, _ := h.store.Tunnel("a")

	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 2*time.Second, "the grace period to promote the status", func() bool {
		return h.m.View(tunnel).Status == StatusRunning
	})

	view := h.m.View(tunnel)
	if view.PID == nil || *view.PID != h.sp.last().PID() {
		t.Fatalf("pid = %v, want %d", view.PID, h.sp.last().PID())
	}
	if view.StartedAt == nil {
		t.Fatal("startedAt must be set while running")
	}

	h.sp.last().exit(exitInfo{Code: 3})
	waitFor(t, 2*time.Second, "the exit to reach the view", func() bool {
		return h.m.View(tunnel).LastExitCode != nil
	})
	view = h.m.View(tunnel)
	if *view.LastExitCode != 3 {
		t.Fatalf("lastExitCode = %d, want 3", *view.LastExitCode)
	}
	if view.StoppedAt == nil || view.LastExitAt == nil {
		t.Fatal("stoppedAt and lastExitAt must be set after an exit")
	}
	if view.PID != nil {
		t.Fatalf("pid = %v, want null after exit", *view.PID)
	}
}

func TestViewCopiesDNSResults(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	tunnel, _ := h.store.Tunnel("a")

	h.withRec("a", func(r *rec) {
		r.dnsStatus = dnsStatusOK
		r.dnsMessage = "已配置 1 个域名"
		r.dnsResults = &DNSResults{
			Routed:    []string{"nas.example.com"},
			AlreadyOK: []string{},
			Failed:    nil,
		}
	})

	view := h.m.View(tunnel)
	if view.DNSStatus != dnsStatusOK || view.DNSMessage != "已配置 1 个域名" {
		t.Fatalf("dns fields wrong: %+v", view)
	}
	if view.DNSResults == nil {
		t.Fatal("dnsResults must be present")
	}
	if len(view.DNSResults.Routed) != 1 || view.DNSResults.Routed[0] != "nas.example.com" {
		t.Fatalf("routed = %v", view.DNSResults.Routed)
	}
	// A nil slice must be normalised so the UI always sees arrays.
	raw, err := json.Marshal(view.DNSResults)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"failed":[]`) {
		t.Fatalf("failed must encode as [], got: %s", raw)
	}

	// The view must be a copy: mutating it must not touch the record.
	view.DNSResults.Routed[0] = "changed"
	again := h.m.View(tunnel)
	if again.DNSResults.Routed[0] != "nas.example.com" {
		t.Fatal("View must copy the DNS results, not alias them")
	}
}
