package cf

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// stubNetwork replaces the DNS/TCP seams for the duration of one test.
func stubNetwork(t *testing.T, lookup func(string) ([]string, error), dialErr error) {
	t.Helper()
	prevLookup, prevDial := lookupHostFn, dialContextFn
	t.Cleanup(func() { lookupHostFn, dialContextFn = prevLookup, prevDial })

	lookupHostFn = func(_ context.Context, host string) ([]string, error) { return lookup(host) }
	dialContextFn = func(_ context.Context, _, _ string) (net.Conn, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		return fakeConn{}, nil
	}
}

type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)         { return 0, nil }
func (fakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return nil }
func (fakeConn) RemoteAddr() net.Addr             { return nil }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

func TestDiagnoseAllGreen(t *testing.T) {
	stubNetwork(t, func(string) ([]string, error) { return []string{"104.16.0.1"}, nil }, nil)

	got := Diagnose(time.Second)
	if !got.OK {
		t.Fatalf("OK = false with a healthy network: %+v", got)
	}
	if len(got.Checks) != len(loginHosts)+1 {
		t.Fatalf("checks = %d, want %d", len(got.Checks), len(loginHosts)+1)
	}
	for _, c := range got.Checks {
		if !c.OK || c.Detail == "" {
			t.Fatalf("check %q = %+v", c.Name, c)
		}
	}
}

// The real NAS case: the gateway refused the DNS query outright.
func TestDiagnoseReportsDNSError(t *testing.T) {
	stubNetwork(t, func(string) ([]string, error) {
		return nil, errors.New(`lookup login.cloudflareaccess.org on 192.168.80.1:53: ` +
			`read udp 192.168.80.128:42099->192.168.80.1:53: read: connection refused`)
	}, nil)

	got := Diagnose(time.Second)
	if got.OK {
		t.Fatal("OK = true although every lookup failed")
	}
	if !strings.Contains(got.Checks[0].Detail, "connection refused") {
		t.Fatalf("detail = %q", got.Checks[0].Detail)
	}
	if !looksLikeDNSError(got.Checks[0].Detail) {
		t.Fatal("a refused DNS query must be recognised as a DNS problem")
	}
}

func TestDiagnoseReportsTCPFailure(t *testing.T) {
	stubNetwork(t, func(string) ([]string, error) { return []string{"104.16.0.1"}, nil },
		errors.New("dial tcp 104.16.0.1:443: i/o timeout"))

	got := Diagnose(time.Second)
	if got.OK {
		t.Fatal("OK = true although the TCP dial failed")
	}
	last := got.Checks[len(got.Checks)-1]
	if last.OK || !strings.Contains(last.Detail, "timeout") {
		t.Fatalf("last check = %+v", last)
	}
}

func TestLooksLikeDNSError(t *testing.T) {
	yes := []string{
		"lookup api.cloudflare.com on 192.168.80.1:53: read: connection refused",
		"lookup login.cloudflareaccess.org: no such host",
		"lookup api.cloudflare.com: i/o timeout",
		"lookup x: server misbehaving",
	}
	for _, detail := range yes {
		if !looksLikeDNSError(detail) {
			t.Errorf("not recognised as DNS: %q", detail)
		}
	}
	no := []string{
		"TLS handshake timeout",
		"bind: address already in use",
		"",
	}
	for _, detail := range no {
		if looksLikeDNSError(detail) {
			t.Errorf("wrongly recognised as DNS: %q", detail)
		}
	}
}
