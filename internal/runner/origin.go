package runner

import (
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/config"
)

// Origin health has two independent sources, because neither one alone covers
// every tunnel type:
//
//   - Active probing works only where the ingress rules are known locally
//     (locally-managed and custom-config tunnels). A token tunnel receives its
//     ingress from the Cloudflare dashboard, so this app never sees them.
//   - Log parsing works for every tunnel type, because cloudflared reports the
//     origin it could not reach regardless of where the configuration came
//     from.
//
// The distinction that matters to a user is "the tunnel is connected to
// Cloudflare" versus "the service behind it is answering". cloudflared's
// "running" state only means the former, and a tunnel that is perfectly healthy
// at the edge still returns 502 when the origin is down.

// OriginStatus is the result of probing one ingress origin.
type OriginStatus struct {
	Hostname string `json:"hostname"`
	Service  string `json:"service"`
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
}

// OriginError is the most recent "cannot reach the origin" error cloudflared
// reported. It is kept rather than cleared: a request that succeeded produces
// no log line at the default level, so there is no reliable signal to clear it
// with. The timestamp lets the UI say how stale it is.
type OriginError struct {
	Service string `json:"service"`
	Message string `json:"message"`
	At      string `json:"at"`
}

var (
	// cloudflared logs:
	//   ... error="Unable to reach the origin service. ...: dial tcp
	//       127.0.0.1:7863: connect: connection refused" ... originService=http://127.0.0.1:7863
	// The originService field is the machine-readable half, so match on it and
	// require the human-readable reason to be present too.
	originUnreachableRE = regexp.MustCompile(`Unable to reach the origin service`)
	originServiceRE     = regexp.MustCompile(`originService=(\S+)`)
)

const (
	// A refused connection is instant; a black-holed one is not. Anything slower
	// than this is reported as a timeout, which is still a useful answer.
	originProbeTimeout = 2 * time.Second
	// Re-probing an idle origin on every UI poll would be wasteful, and probing
	// it too rarely hides a service that just died.
	originProbeEvery = 30 * time.Second
)

// originTarget turns an ingress service URL into a dialable host:port. It
// returns false for services that are not TCP endpoints (http_status:404,
// unix sockets, and so on), which must not be reported as failures.
func originTarget(service string) (string, bool) {
	raw := strings.TrimSpace(service)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	switch u.Scheme {
	case "http", "ws":
		if u.Port() == "" {
			return net.JoinHostPort(u.Hostname(), "80"), true
		}
	case "https", "wss":
		if u.Port() == "" {
			return net.JoinHostPort(u.Hostname(), "443"), true
		}
	case "tcp":
		if u.Port() == "" {
			return "", false
		}
	default:
		// http_status:404, hello_world, unix:/path, ssh://, and anything else
		// cloudflared can route to: not a TCP endpoint we can dial.
		return "", false
	}
	return net.JoinHostPort(u.Hostname(), u.Port()), true
}

// probeTargets lists the origins worth probing for one tunnel.
func probeTargets(t config.Tunnel) []OriginStatus {
	out := make([]OriginStatus, 0, len(t.Ingress))
	for _, rule := range t.Ingress {
		if _, ok := originTarget(rule.Service); !ok {
			continue
		}
		out = append(out, OriginStatus{
			Hostname: rule.Hostname,
			Service:  rule.Service,
			// Until probed, nothing is known: do not claim either state.
			OK:    true,
			Error: "",
		})
	}
	return out
}

// ProbeOrigins dials every known origin of one tunnel and records the result.
// An unknown tunnel id yields nil and creates no runtime record.
func (m *Manager) ProbeOrigins(id string) []OriginStatus {
	if !m.known(id) {
		return nil
	}
	tunnel, ok := m.store.Tunnel(id)
	if !ok {
		return nil
	}
	results := m.probe(tunnel)

	m.mu.Lock()
	r := m.recordLocked(id)
	r.origins = results
	r.originsAt = m.now()
	m.mu.Unlock()
	return results
}

// probe dials the targets concurrently: a dead origin usually means a refused
// connection (instant), but a black-holed one would otherwise serialise the
// whole list behind a 2-second timeout each.
func (m *Manager) probe(t config.Tunnel) []OriginStatus {
	results := probeTargets(t)
	if len(results) == 0 {
		return results
	}

	var wg sync.WaitGroup
	for i := range results {
		hostport, ok := originTarget(results[i].Service)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(idx int, addr string) {
			defer wg.Done()
			conn, err := m.dial(addr, originProbeTimeout)
			if err != nil {
				results[idx].OK = false
				results[idx].Error = dialErrorText(err)
				return
			}
			_ = conn.Close()
			results[idx].OK = true
			results[idx].Error = ""
		}(i, hostport)
	}
	wg.Wait()
	return results
}

// dialErrorText reduces a dial failure to the part a user can act on. The Go
// error for a refused connection already reads "connection refused", which is
// exactly what the UI should show.
func dialErrorText(err error) string {
	if err == nil {
		return ""
	}
	var netErr net.Error
	if ok := asNetError(err, &netErr); ok && netErr.Timeout() {
		return "连接超时"
	}
	msg := err.Error()
	// Trim the "dial tcp 127.0.0.1:7863: " prefix: the address is already shown
	// next to it, and repeating it buries the reason.
	if idx := strings.LastIndex(msg, ": "); idx >= 0 {
		return msg[idx+2:]
	}
	return msg
}

func asNetError(err error, target *net.Error) bool {
	if ne, ok := err.(net.Error); ok {
		*target = ne
		return true
	}
	return false
}

// watchOrigins keeps a running tunnel's origin status fresh until the child
// exits. It is started once per child and stops with it.
func (m *Manager) watchOrigins(id string, r *rec, rn *run, t config.Tunnel) {
	if len(probeTargets(t)) == 0 {
		// Nothing to dial (token tunnel, or no TCP origins): leave the record
		// untouched so the UI shows "unknown" rather than a false failure.
		return
	}
	go func() {
		m.refreshOrigins(id, r, rn, t)
		ticker := time.NewTicker(originProbeEvery)
		defer ticker.Stop()
		for {
			select {
			case <-rn.done:
				return
			case <-ticker.C:
				m.refreshOrigins(id, r, rn, t)
			}
		}
	}()
}

func (m *Manager) refreshOrigins(id string, r *rec, rn *run, t config.Tunnel) {
	results := m.probe(t)

	m.mu.Lock()
	defer m.mu.Unlock()
	// A process that has been replaced must not publish results for the tunnel
	// it used to serve.
	if r.child != rn {
		return
	}
	r.origins = results
	r.originsAt = m.now()
}

// noteOriginError records a "cannot reach the origin" line from cloudflared.
// Called with Manager.mu held.
func (m *Manager) noteOriginErrorLocked(r *rec, line string) {
	if !originUnreachableRE.MatchString(line) {
		return
	}
	service := ""
	if mt := originServiceRE.FindStringSubmatch(line); len(mt) > 1 {
		service = mt[1]
	}
	r.originErr = &OriginError{
		Service: service,
		Message: truncateRunes(originReason(line), errorSampleRunes),
		At:      m.now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

// originReason pulls the actionable part out of the log line: the text after
// the "Unable to reach the origin service. ...: " preamble.
func originReason(line string) string {
	const marker = "Unable to reach the origin service"
	idx := strings.Index(line, marker)
	if idx < 0 {
		return strings.TrimSpace(line)
	}
	rest := line[idx:]
	// Drop the trailing key=value fields cloudflared appends.
	for _, cut := range []string{" connIndex=", " event=", " ingressRule=", " originService="} {
		if at := strings.Index(rest, cut); at >= 0 {
			rest = rest[:at]
		}
	}
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSuffix(rest, `"`)
	return rest
}
