package runner

import (
	"fmt"

	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
)

// ApplyLocalConfig makes a saved change take effect for a tunnel that is not
// running: without it the config.yml and the DNS routes stay as they were at
// the last start, so a hostname added in the UI looks saved (and the DNS panel
// looks healthy) while nothing routes to it.
func (m *Manager) ApplyLocalConfig(t config.Tunnel) {
	if t.Type != config.TypeLocal {
		return
	}
	cred := m.cf.ResolveCredentials(t)
	if cred == "" {
		m.warn("tunnel " + t.ID + ": no credentials file, skipping config write after save")
		return
	}
	if _, err := m.cf.WriteTunnelConfig(t, cred); err != nil {
		m.warn("tunnel " + t.ID + ": writing config.yml after save failed: " + err.Error())
		return
	}
	m.RunDNSRoutes(t.ID, true)
}

// RunDNSRoutes checks (and repairs) the CNAME routes of a locally managed
// tunnel in the background, recording the outcome on the runtime record so the
// UI can show it. It never blocks the caller: the tunnel starts regardless.
func (m *Manager) RunDNSRoutes(id string, force bool) {
	tunnel, ok := m.store.Tunnel(id)
	if !ok {
		return
	}

	m.mu.Lock()
	r := m.recordLocked(id)
	running := r.dnsRunning
	m.mu.Unlock()
	if running {
		return
	}

	if tunnel.Type != config.TypeLocal || !tunnel.AutoDNS {
		return
	}
	hostnames := m.dns.Hostnames(tunnel)
	if len(hostnames) == 0 {
		return
	}

	// Auto-restarts can fire every few seconds; only re-check when the cache is
	// stale, otherwise a crash loop would hammer the Cloudflare API.
	if !force {
		name := tunnel.TunnelName
		if name == "" {
			name = tunnel.TunnelID
		}
		fresh := true
		for _, hostname := range hostnames {
			if !m.dns.CacheFresh(name, hostname) {
				fresh = false
			}
		}
		if fresh {
			return
		}
	}

	m.mu.Lock()
	// Re-check under the lock: two callers can both pass the check above.
	if r.dnsRunning {
		m.mu.Unlock()
		return
	}
	r.dnsRunning = true
	r.dnsStatus = dnsStatusChecking
	r.dnsMessage = "正在检查 DNS 路由…"
	m.mu.Unlock()

	go func() {
		defer func() {
			if p := recover(); p != nil {
				msg := "DNS 路由检查失败: " + fmt.Sprint(p)
				m.mu.Lock()
				r.dnsStatus = dnsStatusError
				r.dnsMessage = msg
				r.dnsRunning = false
				m.mu.Unlock()
				m.warn("dns route check panicked for " + id + ": " + fmt.Sprint(p))
			}
		}()

		res := m.dns.Ensure(tunnel)

		m.mu.Lock()
		r.dnsResults = resultsFrom(res)
		r.dnsMessage = res.Message
		switch {
		case res.Skipped:
			r.dnsStatus = dnsStatusSkipped
		case res.OK:
			r.dnsStatus = dnsStatusOK
		default:
			r.dnsStatus = dnsStatusError
		}
		r.dnsRunning = false
		m.mu.Unlock()

		if len(res.Routed) > 0 || len(res.Failed) > 0 {
			m.info("dns routes for " + id + ": " + res.Message)
		}
	}()
}

// SetDNSResult records the outcome of a check the HTTP layer ran itself (the
// manual "repair DNS routes" action), so the tunnel view stays consistent with
// whatever the user just triggered.
func (m *Manager) SetDNSResult(id string, res dnsroute.Result) {
	if !m.known(id) {
		// An unknown id must not leave a runtime record behind (invariant 10).
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.recordLocked(id)
	r.dnsResults = resultsFrom(res)
	r.dnsMessage = res.Message
	switch {
	case res.Skipped:
		r.dnsStatus = dnsStatusSkipped
	case res.OK:
		r.dnsStatus = dnsStatusOK
	default:
		r.dnsStatus = dnsStatusError
	}
}

func resultsFrom(res dnsroute.Result) *DNSResults {
	return &DNSResults{
		Routed:    nonNilStrings(res.Routed),
		AlreadyOK: nonNilStrings(res.AlreadyOK),
		Failed:    nonNilFailures(res.Failed),
	}
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return append([]string(nil), in...)
}

func nonNilFailures(in []dnsroute.Failure) []dnsroute.Failure {
	if in == nil {
		return []dnsroute.Failure{}
	}
	return append([]dnsroute.Failure(nil), in...)
}
