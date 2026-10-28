// Package dnsroute keeps the DNS side of a locally-managed tunnel honest.
//
// A hostname served by a named tunnel must resolve as a CNAME to
// <uuid>.cfargotunnel.com. A plain A record -- even an orange-cloud proxied one
// -- never reaches the tunnel, and the edge answers 1033/530 while the tunnel
// itself looks perfectly healthy. A local-managed tunnel knows its own name and
// UUID, so the route can be created and repaired here.
package dnsroute

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/cf"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

const (
	// cacheTTL is how long a verified route is trusted before it is checked
	// again. It is only a rate limiter: the cache lives in memory, so a restart
	// simply re-verifies every route once.
	cacheTTL = 10 * time.Minute
	// lookupTimeout keeps a hanging DNS query from pinning a tunnel's route
	// check forever. Callers guard against re-entry, so a stuck lookup would
	// otherwise leave the tunnel permanently "checking".
	lookupTimeout = 10 * time.Second
	// dohURL is Cloudflare's JSON DNS endpoint.
	dohURL = "https://cloudflare-dns.com/dns-query"
	// cnameType is the DNS record type of a CNAME in a DoH JSON answer.
	cnameType = 5
)

// Failure is one hostname that could not be routed.
type Failure struct {
	Hostname string `json:"hostname"`
	Error    string `json:"error"`
}

// Result is the outcome of one Ensure call. Failed and Message are what the UI
// shows; a failure here never stops the tunnel from starting.
type Result struct {
	OK        bool      `json:"ok"`
	Routed    []string  `json:"routed"`
	AlreadyOK []string  `json:"alreadyOk"`
	Failed    []Failure `json:"failed"`
	Skipped   bool      `json:"skipped"`
	Message   string    `json:"message"`
}

type cacheEntry struct {
	at     time.Time
	target string
}

// Resolver creates and repairs DNS routes for locally-managed tunnels.
type Resolver struct {
	p      paths.Paths
	log    *applog.Logger
	runner *cf.Runner

	client *http.Client

	// Test seams: dohURL lets a test point the lookup at a local server, and
	// routeFn lets it simulate the cloudflared route command. now is the clock
	// the cache is measured against.
	dohEndpoint string
	routeFn     func(name, hostname string) error
	now         func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New returns a Resolver that shells out through runner.
func New(p paths.Paths, log *applog.Logger, runner *cf.Runner) *Resolver {
	return &Resolver{
		p:           p,
		log:         log,
		runner:      runner,
		client:      &http.Client{Timeout: lookupTimeout},
		dohEndpoint: dohURL,
		now:         time.Now,
		cache:       make(map[string]cacheEntry),
	}
}

// Hostnames returns the hostnames of the ingress rules that need a route, in
// rule order and without duplicates. Anything that is not a bare hostname (a
// scheme, a port, a path) is ignored: it cannot be a DNS name.
func (r *Resolver) Hostnames(t config.Tunnel) []string {
	out := make([]string, 0, len(t.Ingress))
	for _, rule := range t.Ingress {
		host := strings.TrimSpace(rule.Hostname)
		if host == "" || !config.DNSHostnameRE.MatchString(host) {
			continue
		}
		duplicate := false
		for _, seen := range out {
			if seen == host {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, host)
		}
	}
	return out
}

// UUID recovers the tunnel UUID from the credentials file name, which is the
// most reliable source when only a name was stored.
//
// ResolveCredentials only answers with provable paths, so a tunnel known by
// name alone yields "" here: callers must resolve the name to a UUID first
// rather than routing to whichever credentials file happens to sort first.
func (r *Resolver) UUID(t config.Tunnel) string {
	if r.runner == nil {
		return ""
	}
	cred := r.runner.ResolveCredentials(t)
	if cred == "" {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(cred), ".json")
	if config.UUIDRE.MatchString(strings.ToLower(base)) {
		return base
	}
	return ""
}

// Ensure creates or repairs the DNS routes of one tunnel. It is synchronous and
// idempotent: every call re-verifies each hostname, so a forced run really
// bypasses the cache. It only ever *writes* the cache (see Remember); deciding
// whether a check is due is the caller's job (see CacheFresh).
func (r *Resolver) Ensure(t config.Tunnel) Result {
	result := Result{
		OK:        true,
		Routed:    []string{},
		AlreadyOK: []string{},
		Failed:    []Failure{},
		Skipped:   true,
	}

	if t.Type != config.TypeLocal || !t.AutoDNS {
		result.Message = "未启用自动 DNS 路由"
		return result
	}
	name := t.TunnelName
	if name == "" {
		name = t.TunnelID
	}
	if name == "" {
		result.OK = false
		result.Message = "未选择已创建的隧道"
		return result
	}
	hostnames := r.Hostnames(t)
	if len(hostnames) == 0 {
		result.Message = "入站规则中没有需要路由的域名"
		return result
	}
	if !fsx.Exists(r.p.CertFile) {
		result.OK = false
		result.Message = "尚未登录 Cloudflare 账户，无法自动配置 DNS 路由"
		return result
	}

	result.Skipped = false
	// Never concatenate an empty id: that produces ".cfargotunnel.com", which is
	// both truthy and impossible to match, and would quietly skew the verdict.
	uuid := t.TunnelID
	if uuid == "" {
		uuid = r.UUID(t)
	}
	if uuid == "" {
		result.OK = false
		result.Message = "无法确认隧道 UUID，跳过 DNS 路由配置"
		return result
	}
	target := uuid + ".cfargotunnel.com"

	for _, hostname := range hostnames {
		current, err := r.LookupCNAME(hostname)
		if err != nil {
			r.warn("dns lookup failed for " + hostname + ": " + err.Error())
		}
		if current != "" && strings.EqualFold(current, target) {
			result.AlreadyOK = append(result.AlreadyOK, hostname)
			r.Remember(name, hostname, current)
			continue
		}

		shown := current
		if shown == "" {
			shown = "none"
		}
		r.info("routing " + hostname + " -> " + name + " (current CNAME: " + shown + ")")

		if err := r.route(name, hostname); err != nil {
			result.Failed = append(result.Failed, Failure{Hostname: hostname, Error: err.Error()})
			continue
		}
		result.Routed = append(result.Routed, hostname)
		r.Remember(name, hostname, target)
	}

	switch {
	case len(result.Failed) > 0:
		result.OK = false
		parts := make([]string, 0, len(result.Failed))
		for _, f := range result.Failed {
			parts = append(parts, f.Hostname+": "+f.Error)
		}
		result.Message = strings.Join(parts, "；")
	case len(result.Routed) > 0:
		result.Message = "已配置 DNS 路由: " + strings.Join(result.Routed, "、")
	default:
		result.Message = "DNS 路由已正确: " + strings.Join(result.AlreadyOK, "、")
	}
	return result
}

// route creates the route through cloudflared.
func (r *Resolver) route(name, hostname string) error {
	if r.routeFn != nil {
		return r.routeFn(name, hostname)
	}
	if r.runner == nil {
		return fmt.Errorf("尚未登录 Cloudflare 账户")
	}
	return r.runner.RouteDNS(name, hostname)
}

// LookupCNAME asks Cloudflare's DoH endpoint for the CNAME of hostname. It
// returns "" and no error when the name has no CNAME at all.
func (r *Resolver) LookupCNAME(hostname string) (string, error) {
	endpoint := r.dohEndpoint
	if endpoint == "" {
		endpoint = dohURL
	}
	req, err := http.NewRequest(http.MethodGet,
		endpoint+"?name="+url.QueryEscape(hostname)+"&type=CNAME", nil)
	if err != nil {
		return "", fmt.Errorf("DNS 查询失败: %s", err.Error())
	}
	req.Header.Set("accept", "application/dns-json")

	res, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("DNS 查询失败: %s", err.Error())
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DNS 查询失败 (HTTP %d)", res.StatusCode)
	}

	var data struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("DNS 查询失败: %s", err.Error())
	}
	for _, answer := range data.Answer {
		if answer.Type == cnameType {
			return strings.TrimSuffix(answer.Data, "."), nil
		}
	}
	return "", nil
}

// Remember records a verified route, which CacheFresh uses to skip a re-check
// for the next ten minutes.
func (r *Resolver) Remember(name, hostname, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[cacheKey(name, hostname)] = cacheEntry{at: r.clock(), target: target}
}

// CacheFresh reports whether the route for this name/hostname pair was verified
// within the last ten minutes.
func (r *Resolver) CacheFresh(name, hostname string) bool {
	r.mu.Lock()
	entry, ok := r.cache[cacheKey(name, hostname)]
	r.mu.Unlock()
	if !ok {
		return false
	}
	// Inclusive, like the previous implementation's `now - at <= TTL`.
	return !r.clock().After(entry.at.Add(cacheTTL))
}

// cacheKey uses NUL, not a newline: "a\nb"+"\n"+"c" and "a"+"\n"+"b\nc" would
// otherwise produce the same key. Neither a tunnel name nor a hostname may
// contain a NUL byte.
func cacheKey(name, hostname string) string { return name + "\x00" + hostname }

func (r *Resolver) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Resolver) info(line string) {
	if r.log != nil {
		r.log.Info(line)
	}
}

func (r *Resolver) warn(line string) {
	if r.log != nil {
		r.log.Warn(line)
	}
}
