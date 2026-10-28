// Package web is the HTTP surface: static assets for the browser UI plus the
// small REST API that drives it.
//
// Nothing may panic out of a request handler. An uncaught panic in the previous
// implementation terminated the process, which orphaned every running
// cloudflared child and left the UI dead until someone restarted the app by
// hand.
package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/cf"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
	"github.com/ybd156/fnos-cloudflared/internal/runner"
)

// Server wires the service modules into one http.Handler.
type Server struct {
	P     paths.Paths
	Log   *applog.Logger
	Store *config.Store
	Cf    *cf.Runner
	DNS   *dnsroute.Resolver
	Run   *runner.Manager

	started time.Time
}

// New returns a Server. startedAt is used for the uptime field of /api/meta.
func New(p paths.Paths, log *applog.Logger, store *config.Store,
	cfRunner *cf.Runner, dns *dnsroute.Resolver, run *runner.Manager) *Server {
	return &Server{P: p, Log: log, Store: store, Cf: cfRunner, DNS: dns, Run: run, started: time.Now()}
}

// Handler returns the request handler, with panic recovery wrapped around the
// whole surface.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("unhandled request panic:", rec)
				// Only answer when nothing has been flushed yet; writing a
				// second status line would corrupt the response.
				if !tw.wrote {
					http.Error(tw, "Internal Error", http.StatusInternalServerError)
				}
			}
		}()
		s.route(tw, r)
	})
}

// trackingWriter remembers whether a status line has gone out, so the panic
// recovery above can tell "answer with 500" from "the client already has a
// partial response".
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(status int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.ContainsRune(path, 0) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	stripped, prefixed := s.stripPrefix(path)

	if stripped == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	if strings.HasPrefix(stripped, "/api/") {
		s.handleAPI(w, r, stripped)
		return
	}

	// Requests that arrive through the fnOS gateway carry the /app/<name>
	// prefix; the HTML placeholder needs it so asset and API URLs stay correct
	// behind the gateway and on a bare port alike.
	base := ""
	if prefixed {
		base = s.P.GatewayPrefix
	}
	s.serveStatic(w, r, stripped, base)
}

// stripPrefix removes the fnOS gateway prefix so the same router serves both
// entry points, and reports whether it was present.
func (s *Server) stripPrefix(path string) (string, bool) {
	prefix := s.P.GatewayPrefix
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return path[len(prefix):], true
	}
	return path, false
}

// apiSegments splits an /api/... path into its components.
func apiSegments(path string) []string {
	rest := strings.TrimPrefix(path, "/api/")
	parts := strings.Split(rest, "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
