package runner

import (
	"errors"
	"testing"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
)

func okResult(hostname string) dnsroute.Result {
	return dnsroute.Result{
		OK:      true,
		Routed:  []string{hostname},
		Message: "已配置 1 个域名",
	}
}

func TestRunDNSRoutesChecksWhenCacheIsStale(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))
	h.dns.setFresh(false)

	h.m.RunDNSRoutes("a", false)

	waitFor(t, 2*time.Second, "the DNS check to finish", func() bool {
		var status string
		h.withRec("a", func(r *rec) { status = r.dnsStatus })
		return status == dnsStatusOK
	})
	if got := h.dns.callCount(); got != 1 {
		t.Fatalf("Ensure called %d times, want 1", got)
	}

	var (
		msg     string
		results *DNSResults
		running bool
	)
	h.withRec("a", func(r *rec) {
		msg = r.dnsMessage
		results = r.dnsResults
		running = r.dnsRunning
	})
	if msg != "已配置 1 个域名" {
		t.Fatalf("dnsMessage = %q", msg)
	}
	if results == nil || len(results.Routed) != 1 || results.Routed[0] != "nas.example.com" {
		t.Fatalf("dnsResults = %+v", results)
	}
	if running {
		t.Fatal("dnsRunning must be false once the check finished")
	}
}

func TestRunDNSRoutesSkipsWhenCacheIsFresh(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.dns.set([]string{"nas.example.com", "www.example.com"}, okResult("nas.example.com"))
	h.dns.setFresh(true)

	h.m.RunDNSRoutes("a", false)

	// The cache covers every hostname, so the Cloudflare API is left alone.
	time.Sleep(50 * time.Millisecond)
	if got := h.dns.callCount(); got != 0 {
		t.Fatalf("Ensure called %d times, want 0 while the cache is fresh", got)
	}
}

func TestRunDNSRoutesForceBypassesCache(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))
	h.dns.setFresh(true)

	h.m.RunDNSRoutes("a", true)

	waitFor(t, 2*time.Second, "the forced check", func() bool {
		return h.dns.callCount() == 1
	})
}

func TestRunDNSRoutesIgnoresNonLocalTunnels(t *testing.T) {
	for _, typ := range []string{config.TypeToken, config.TypeQuick, config.TypeConfig} {
		tunnel := testTunnel("a")
		tunnel.Type = typ
		h := newHarness(t, tunnel)
		h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))
		h.dns.setFresh(false)

		h.m.RunDNSRoutes("a", true)
		time.Sleep(20 * time.Millisecond)
		if got := h.dns.callCount(); got != 0 {
			t.Fatalf("type %s: Ensure called %d times, want 0", typ, got)
		}
	}
}

func TestRunDNSRoutesIgnoresAutoDNSOff(t *testing.T) {
	tunnel := testTunnel("a")
	tunnel.AutoDNS = false
	h := newHarness(t, tunnel)
	h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))
	h.dns.setFresh(false)

	h.m.RunDNSRoutes("a", true)
	time.Sleep(20 * time.Millisecond)
	if got := h.dns.callCount(); got != 0 {
		t.Fatalf("Ensure called %d times, want 0 when autoDns is off", got)
	}
}

func TestRunDNSRoutesIgnoresTunnelsWithoutHostnames(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.dns.set(nil, dnsroute.Result{})
	h.dns.setFresh(false)

	h.m.RunDNSRoutes("a", true)
	time.Sleep(20 * time.Millisecond)
	if got := h.dns.callCount(); got != 0 {
		t.Fatalf("Ensure called %d times, want 0 without hostnames", got)
	}
}

func TestRunDNSRoutesIgnoresUnknownTunnel(t *testing.T) {
	h := newHarness(t)
	h.m.RunDNSRoutes("nope", true)
	if h.hasRecord("nope") {
		t.Fatal("an unknown id must not get a runtime record")
	}
}

func TestRunDNSRoutesIsNotReentrant(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))
	h.dns.setFresh(false)

	gate := make(chan struct{})
	h.dns.setGate(gate)

	h.m.RunDNSRoutes("a", true)
	waitFor(t, 2*time.Second, "the first check to start", func() bool {
		return h.dns.callCount() == 1
	})

	// A second call while the first is still in flight must be dropped, or a
	// crash loop would stack API calls.
	h.m.RunDNSRoutes("a", true)
	time.Sleep(20 * time.Millisecond)
	if got := h.dns.callCount(); got != 1 {
		t.Fatalf("Ensure called %d times, want 1 while a check is in flight", got)
	}

	close(gate)
	waitFor(t, 2*time.Second, "the check to finish", func() bool {
		var running bool
		h.withRec("a", func(r *rec) { running = r.dnsRunning })
		return !running
	})

	// With the guard released a later call runs again.
	h.m.RunDNSRoutes("a", true)
	waitFor(t, 2*time.Second, "the second check", func() bool {
		return h.dns.callCount() == 2
	})
}

func TestRunDNSRoutesRecordsFailure(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.dns.set([]string{"nas.example.com"}, dnsroute.Result{
		OK:      false,
		Failed:  []dnsroute.Failure{{Hostname: "nas.example.com", Error: "无权访问该区域"}},
		Message: "1 个域名失败",
	})
	h.dns.setFresh(false)

	h.m.RunDNSRoutes("a", true)

	waitFor(t, 2*time.Second, "the failure to be recorded", func() bool {
		var status string
		h.withRec("a", func(r *rec) { status = r.dnsStatus })
		return status == dnsStatusError
	})
	var results *DNSResults
	h.withRec("a", func(r *rec) { results = r.dnsResults })
	if results == nil || len(results.Failed) != 1 || results.Failed[0].Error != "无权访问该区域" {
		t.Fatalf("dnsResults = %+v", results)
	}
}

func TestRunDNSRoutesRecordsSkipped(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.dns.set([]string{"nas.example.com"}, dnsroute.Result{
		Skipped: true,
		Message: "未启用自动 DNS 路由",
	})
	h.dns.setFresh(false)

	h.m.RunDNSRoutes("a", true)

	waitFor(t, 2*time.Second, "the skipped status", func() bool {
		var status string
		h.withRec("a", func(r *rec) { status = r.dnsStatus })
		return status == dnsStatusSkipped
	})
}

func TestRunDNSRoutesSurvivesAPanickingResolver(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.dns.set([]string{"nas.example.com"}, dnsroute.Result{})
	h.dns.setFresh(false)
	h.dns.setPanic(errors.New("boom"))

	h.m.RunDNSRoutes("a", true)

	waitFor(t, 2*time.Second, "the panic to be contained", func() bool {
		var (
			status  string
			running bool
		)
		h.withRec("a", func(r *rec) {
			status = r.dnsStatus
			running = r.dnsRunning
		})
		return status == dnsStatusError && !running
	})
	var msg string
	h.withRec("a", func(r *rec) { msg = r.dnsMessage })
	if msg != "DNS 路由检查失败: boom" {
		t.Fatalf("dnsMessage = %q", msg)
	}
}

func TestSetDNSResultUpdatesTheRecord(t *testing.T) {
	h := newHarness(t, testTunnel("a"))

	h.m.SetDNSResult("a", okResult("nas.example.com"))

	var (
		status  string
		message string
		results *DNSResults
	)
	h.withRec("a", func(r *rec) {
		status = r.dnsStatus
		message = r.dnsMessage
		results = r.dnsResults
	})
	if status != dnsStatusOK || message != "已配置 1 个域名" {
		t.Fatalf("status = %q, message = %q", status, message)
	}
	if results == nil || len(results.Routed) != 1 {
		t.Fatalf("results = %+v", results)
	}

	h.m.SetDNSResult("a", dnsroute.Result{Skipped: true, Message: "未启用自动 DNS 路由"})
	h.withRec("a", func(r *rec) { status = r.dnsStatus })
	if status != dnsStatusSkipped {
		t.Fatalf("status = %q, want %q", status, dnsStatusSkipped)
	}

	h.m.SetDNSResult("a", dnsroute.Result{OK: false, Message: "1 个域名失败"})
	h.withRec("a", func(r *rec) { status = r.dnsStatus })
	if status != dnsStatusError {
		t.Fatalf("status = %q, want %q", status, dnsStatusError)
	}
}

func TestApplyLocalConfigWritesThenChecksDNS(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.cf.setCred("/var/apps/cloudflared/var/.cloudflared/abc.json")
	h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))
	h.dns.setFresh(true) // forced by ApplyLocalConfig, so the cache must not skip it

	tunnel, _ := h.store.Tunnel("a")
	h.m.ApplyLocalConfig(tunnel)

	if got := h.cf.writeCount(); got != 1 {
		t.Fatalf("WriteTunnelConfig called %d times, want 1", got)
	}
	waitFor(t, 2*time.Second, "the forced DNS check", func() bool {
		return h.dns.callCount() == 1
	})
}

func TestApplyLocalConfigSkipsWithoutCredentials(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.cf.setCred("")
	h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))
	h.dns.setFresh(false)

	tunnel, _ := h.store.Tunnel("a")
	h.m.ApplyLocalConfig(tunnel)

	if got := h.cf.writeCount(); got != 0 {
		t.Fatalf("WriteTunnelConfig called %d times without credentials, want 0", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := h.dns.callCount(); got != 0 {
		t.Fatalf("Ensure called %d times without credentials, want 0", got)
	}
}

func TestApplyLocalConfigStopsWhenTheWriteFails(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.cf.setCred("/var/apps/cloudflared/var/.cloudflared/abc.json")
	h.cf.setWriteErr(errors.New("磁盘已满"))
	h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))
	h.dns.setFresh(false)

	tunnel, _ := h.store.Tunnel("a")
	h.m.ApplyLocalConfig(tunnel)

	time.Sleep(20 * time.Millisecond)
	if got := h.dns.callCount(); got != 0 {
		t.Fatalf("Ensure called %d times after a failed config write, want 0", got)
	}
}

func TestApplyLocalConfigIgnoresNonLocalTunnels(t *testing.T) {
	tunnel := testTunnel("a")
	tunnel.Type = config.TypeToken
	h := newHarness(t, tunnel)
	h.cf.setCred("/somewhere/creds.json")

	stored, _ := h.store.Tunnel("a")
	h.m.ApplyLocalConfig(stored)

	if got := h.cf.writeCount(); got != 0 {
		t.Fatalf("WriteTunnelConfig called %d times for a token tunnel, want 0", got)
	}
}
