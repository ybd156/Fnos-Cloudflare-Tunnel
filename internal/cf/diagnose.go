package cf

import (
	"context"
	"net"
	"strings"
	"time"
)

// Check is one line of the network self-test.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Diagnosis is the whole self-test: whether cloudflared can reach the hosts the
// login flow needs.
type Diagnosis struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// loginHosts are the names cloudflared must resolve to authorise an account and
// to reach the API afterwards.
var loginHosts = []string{
	"login.cloudflareaccess.org",
	"api.cloudflare.com",
	"region1.v2.argotunnel.com",
}

// Seams, so the checks can be exercised without touching the network.
var (
	lookupHostFn = func(ctx context.Context, host string) ([]string, error) {
		return net.DefaultResolver.LookupHost(ctx, host)
	}
	dialContextFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
)

// Diagnose answers "why can't this NAS log in" with facts instead of a guess.
//
// A real NAS reported `lookup login.cloudflareaccess.org on 192.168.80.1:53:
// read udp ...: connection refused` -- the gateway refused the DNS query
// outright. Nothing in the app can fix that (cloudflared is a static Go binary
// and resolves through /etc/resolv.conf), but the user can fix their DNS once
// they can see which step failed and where the query went.
func Diagnose(timeout time.Duration) Diagnosis {
	out := Diagnosis{OK: true, Checks: []Check{}}

	for _, host := range loginHosts {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		addrs, err := lookupHostFn(ctx, host)
		cancel()

		check := Check{Name: "DNS " + host}
		if err != nil {
			check.OK = false
			check.Detail = err.Error()
			out.OK = false
		} else {
			check.OK = true
			check.Detail = strings.Join(addrs, ", ")
		}
		out.Checks = append(out.Checks, check)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	check := Check{Name: "TCP api.cloudflare.com:443"}
	if conn, err := dialContextFn(ctx, "tcp", "api.cloudflare.com:443"); err != nil {
		check.OK = false
		check.Detail = err.Error()
		out.OK = false
	} else {
		check.OK = true
		check.Detail = "可连接"
		_ = conn.Close()
	}
	out.Checks = append(out.Checks, check)

	return out
}

// looksLikeDNSError reports whether cloudflared's own words describe a name
// resolution failure, so the login error can name the real culprit instead of
// leaving the user with a Go stack trace.
func looksLikeDNSError(detail string) bool {
	lower := strings.ToLower(detail)
	if !strings.Contains(lower, "lookup ") && !strings.Contains(lower, "no such host") {
		return false
	}
	for _, hint := range []string{"refused", "no such host", "timeout", "server misbehaving", "i/o timeout", "network is unreachable"} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}
