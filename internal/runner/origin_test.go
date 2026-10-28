package runner

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/config"
)

/* ------------------------------------------------------------ 目标解析 */

func TestOriginTarget(t *testing.T) {
	cases := []struct {
		service string
		want    string
		ok      bool
	}{
		{"http://127.0.0.1:7863", "127.0.0.1:7863", true},
		{"http://localhost:5000", "localhost:5000", true},
		{"https://nas.local:8443", "nas.local:8443", true},
		// Default ports are filled in, so the dial address is complete.
		{"http://example.com", "example.com:80", true},
		{"https://example.com", "example.com:443", true},
		// A path must not leak into the dial address.
		{"http://127.0.0.1:8080/base/path", "127.0.0.1:8080", true},
		// Not TCP endpoints: must be skipped, not reported as failures.
		{"http_status:404", "", false},
		{"hello_world", "", false},
		{"unix:/var/run/app.sock", "", false},
		{"", "", false},
		{"tcp://127.0.0.1", "", false}, // no port for a bare tcp service
	}
	for _, tc := range cases {
		got, ok := originTarget(tc.service)
		if ok != tc.ok {
			t.Errorf("originTarget(%q) ok = %v, want %v", tc.service, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("originTarget(%q) = %q, want %q", tc.service, got, tc.want)
		}
	}
}

func TestProbeTargetsSkipsNonTCP(t *testing.T) {
	tunnel := config.Tunnel{
		Ingress: []config.Ingress{
			{Hostname: "a.example.com", Service: "http://127.0.0.1:7863"},
			{Hostname: "b.example.com", Service: "http_status:404"},
			{Hostname: "c.example.com", Service: "https://127.0.0.1:443"},
		},
	}
	got := probeTargets(tunnel)
	if len(got) != 2 {
		t.Fatalf("应只探测 2 个 TCP 源站，得到 %d: %+v", len(got), got)
	}
	if got[0].Hostname != "a.example.com" || got[1].Hostname != "c.example.com" {
		t.Errorf("探测目标不符: %+v", got)
	}
}

func TestProbeTargetsEmptyForTokenTunnel(t *testing.T) {
	// A token tunnel's ingress lives in the Cloudflare dashboard, so this app
	// knows no origins for it. That must be "unknown", never "failed".
	tunnel := config.Tunnel{Type: config.TypeToken}
	if got := probeTargets(tunnel); len(got) != 0 {
		t.Errorf("Token 隧道不应有本地探测目标，得到 %+v", got)
	}
}

/* ------------------------------------------------------------ 探测结果 */

// fakeDial returns a dial function that reports every address as reachable
// except the ones listed in refused.
func fakeDial(refused map[string]string) func(string, time.Duration) (net.Conn, error) {
	return func(addr string, _ time.Duration) (net.Conn, error) {
		if msg, bad := refused[addr]; bad {
			return nil, errors.New(msg)
		}
		client, server := net.Pipe()
		// The caller only closes the connection; the peer must not leak.
		go func() { _ = server.Close() }()
		return client, nil
	}
}

func TestProbeReportsReachableOrigin(t *testing.T) {
	h := newHarness(t, localTunnelWithIngress("a", "a.example.com", "http://127.0.0.1:7863"))
	h.m.dial = fakeDial(nil)

	got := h.m.ProbeOrigins("a")
	if len(got) != 1 {
		t.Fatalf("应返回 1 条，得到 %+v", got)
	}
	if !got[0].OK || got[0].Error != "" {
		t.Errorf("可连的源站应报告 ok，得到 %+v", got[0])
	}
	if got[0].Service != "http://127.0.0.1:7863" || got[0].Hostname != "a.example.com" {
		t.Errorf("结果应带上域名与服务地址，得到 %+v", got[0])
	}
}

// TestProbeReportsRefusedOrigin is the case this feature exists for: the tunnel
// is fine, the origin is not.
func TestProbeReportsRefusedOrigin(t *testing.T) {
	h := newHarness(t, localTunnelWithIngress("a", "aikey.example.com", "http://127.0.0.1:7863"))
	h.m.dial = fakeDial(map[string]string{
		"127.0.0.1:7863": "dial tcp 127.0.0.1:7863: connect: connection refused",
	})

	got := h.m.ProbeOrigins("a")
	if len(got) != 1 || got[0].OK {
		t.Fatalf("不可连的源站应报告失败，得到 %+v", got)
	}
	// The address is already displayed next to the message, so the redundant
	// "dial tcp …: connect: " prefix is stripped down to the reason.
	if got[0].Error != "connection refused" {
		t.Errorf("错误文案应只保留原因，得到 %q", got[0].Error)
	}
}

func TestProbeReportsTimeout(t *testing.T) {
	h := newHarness(t, localTunnelWithIngress("a", "a.example.com", "http://10.0.0.1:9"))
	h.m.dial = func(string, time.Duration) (net.Conn, error) {
		return nil, timeoutError{}
	}
	got := h.m.ProbeOrigins("a")
	if len(got) != 1 || got[0].OK {
		t.Fatalf("超时应报告失败，得到 %+v", got)
	}
	if got[0].Error != "连接超时" {
		t.Errorf("超时文案不符: %q", got[0].Error)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestProbeUnknownTunnelLeavesNoRecord(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if got := h.m.ProbeOrigins("ghost"); got != nil {
		t.Errorf("未知 id 应返回 nil，得到 %+v", got)
	}
	h.m.mu.Lock()
	_, exists := h.m.recs["ghost"]
	h.m.mu.Unlock()
	if exists {
		t.Error("未知 id 不应在监督器里留下记录")
	}
}

func TestProbeResultsAppearInView(t *testing.T) {
	h := newHarness(t, localTunnelWithIngress("a", "a.example.com", "http://127.0.0.1:7863"))
	h.m.dial = fakeDial(map[string]string{"127.0.0.1:7863": "connect: connection refused"})
	h.m.ProbeOrigins("a")

	tunnel, _ := h.store.Tunnel("a")
	view := h.m.View(tunnel)
	if len(view.Origins) != 1 {
		t.Fatalf("View 应带上探测结果，得到 %+v", view.Origins)
	}
	if view.Origins[0].OK {
		t.Error("View 里的结果应为失败")
	}
}

func TestViewOriginsIsAlwaysAnArray(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	tunnel, _ := h.store.Tunnel("a")
	view := h.m.View(tunnel)
	if view.Origins == nil {
		t.Error("origins 必须是空数组而不是 null")
	}
	if len(view.Origins) != 0 {
		t.Errorf("从未探测时应为空，得到 %+v", view.Origins)
	}
}

/* -------------------------------------------------------- 日志解析源站错误 */

// cloudflaredOriginLine is a verbatim line from a real NAS log.
const cloudflaredOriginLine = `[07:36:35] 2026-10-28T07:36:35Z ERR  error="Unable to reach the origin service. The service may be down or it may not be responding to traffic from cloudflared: dial tcp 127.0.0.1:7863: connect: connection refused" connIndex=3 event=1 ingressRule=2 originService=http://127.0.0.1:7863`

func TestOriginErrorParsedFromLog(t *testing.T) {
	h := newHarness(t, localTunnelWithIngress("a", "aikey.example.com", "http://127.0.0.1:7863"))
	h.m.mu.Lock()
	r := h.m.recordLocked("a")
	h.m.noteOriginErrorLocked(r, cloudflaredOriginLine)
	h.m.mu.Unlock()

	tunnel, _ := h.store.Tunnel("a")
	view := h.m.View(tunnel)
	if view.OriginError == nil {
		t.Fatal("应从日志里解析出源站错误")
	}
	if view.OriginError.Service != "http://127.0.0.1:7863" {
		t.Errorf("service 解析不符: %q", view.OriginError.Service)
	}
	if !strings.Contains(view.OriginError.Message, "connection refused") {
		t.Errorf("message 应包含真实原因: %q", view.OriginError.Message)
	}
	// The trailing key=value fields must not end up in the message.
	for _, noise := range []string{"connIndex=", "ingressRule=", "originService="} {
		if strings.Contains(view.OriginError.Message, noise) {
			t.Errorf("message 不应包含 %q: %q", noise, view.OriginError.Message)
		}
	}
	if view.OriginError.At == "" {
		t.Error("应记录发生时间")
	}
}

// TestOriginErrorWorksForTokenTunnel is the reason log parsing exists: a token
// tunnel's ingress is configured in the dashboard, so this app cannot probe it,
// but cloudflared still reports the origin it failed to reach.
func TestOriginErrorWorksForTokenTunnel(t *testing.T) {
	h := newHarness(t, testTunnel("a")) // token tunnel, no local ingress
	h.m.mu.Lock()
	h.m.noteOriginErrorLocked(h.m.recordLocked("a"), cloudflaredOriginLine)
	h.m.mu.Unlock()

	tunnel, _ := h.store.Tunnel("a")
	view := h.m.View(tunnel)
	if view.OriginError == nil {
		t.Fatal("Token 隧道也应能从日志里拿到源站错误")
	}
	if len(view.Origins) != 0 {
		t.Error("Token 隧道没有本地 ingress，探测结果应为空")
	}
}

func TestOriginErrorIgnoresUnrelatedLines(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.m.mu.Lock()
	r := h.m.recordLocked("a")
	for _, line := range []string{
		"Registered tunnel connection connIndex=0",
		"INF ICMP proxy will use 192.168.250.3 as source for IPv4",
		"ERR failed to connect to edge: i/o timeout",
	} {
		h.m.noteOriginErrorLocked(r, line)
	}
	h.m.mu.Unlock()
	if r.originErr != nil {
		t.Errorf("无关日志不应产生源站错误，得到 %+v", r.originErr)
	}
}

func TestOriginErrorClearedOnRestart(t *testing.T) {
	h := newHarness(t, localTunnelWithIngress("a", "a.example.com", "http://127.0.0.1:7863"))
	h.m.dial = fakeDial(nil)

	h.m.mu.Lock()
	h.m.recordLocked("a").originErr = &OriginError{Service: "x", Message: "old", At: "t"}
	h.m.mu.Unlock()

	// A restart is a new attempt; the previous attempt's origin error no longer
	// describes the running process.
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	waitFor(t, 2*time.Second, "源站错误被清空", func() bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return h.m.recs["a"].originErr == nil
	})
}

/* ------------------------------------------------------------------ 助手 */

func localTunnelWithIngress(id, hostname, service string) config.Tunnel {
	t := config.Tunnel{
		ID:         id,
		Name:       id,
		Type:       config.TypeLocal,
		Enabled:    true,
		AutoStart:  true,
		TunnelName: "nas",
		Ingress:    []config.Ingress{{Hostname: hostname, Service: service}},
	}
	return t
}
