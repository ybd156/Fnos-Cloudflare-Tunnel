package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
	"github.com/ybd156/fnos-cloudflared/internal/runner"
)

// handleTunnels serves /api/tunnels and /api/tunnels/<id>[/<action>].
func (s *Server) handleTunnels(w http.ResponseWriter, r *http.Request, id, action string) {
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			s.listTunnels(w)
		case http.MethodPost:
			s.createTunnel(w, r)
		default:
			sendError(w, http.StatusNotFound, "接口不存在")
		}
		return
	}

	if action != "" {
		switch action {
		case "start":
			s.startTunnel(w, r, id)
		case "stop":
			s.stopTunnel(w, r, id)
		case "dns":
			s.repairDNS(w, r, id)
		case "restart":
			s.restartTunnel(w, r, id)
		case "logs":
			s.tunnelLogs(w, r, id)
		case "probe":
			s.probeOrigin(w, r, id)
		default:
			sendError(w, http.StatusNotFound, "接口不存在")
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getTunnel(w, r, id)
	case http.MethodPut:
		s.updateTunnel(w, r, id)
	case http.MethodDelete:
		s.deleteTunnel(w, r, id)
	default:
		sendError(w, http.StatusNotFound, "接口不存在")
	}
}

func (s *Server) listTunnels(w http.ResponseWriter) {
	tunnels := s.Store.Tunnels()
	views := make([]any, 0, len(tunnels))
	for _, t := range tunnels {
		views = append(views, s.Run.View(t))
	}
	sendJSON(w, http.StatusOK, map[string]any{"tunnels": views})
}

func (s *Server) createTunnel(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		sendError(w, statusOf(err), err.Error())
		return
	}
	merged, err := config.MergeTunnel(nil, body)
	if err != nil {
		sendError(w, statusOf(err), err.Error())
		return
	}
	if problems := config.ValidateTunnel(merged); len(problems) > 0 {
		sendError(w, http.StatusBadRequest, strings.Join(problems, "；"))
		return
	}
	merged.ID = config.NewID()
	s.resolveLocalTunnel(&merged)
	s.Store.Add(merged)
	if err := s.Store.Save(); err != nil {
		sendError(w, http.StatusInternalServerError, "保存隧道失败："+err.Error())
		return
	}
	s.Log.Info("created tunnel", merged.ID, "("+merged.Type+")")

	// "Auto-start" used to apply only when the service itself started, so a
	// freshly saved tunnel showed as stopped and looked like it had not taken
	// effect.
	if merged.Enabled && merged.AutoStart {
		if err := s.Run.Start(merged.ID); err != nil {
			s.Log.Warn("starting tunnel", merged.ID, "after create:", err.Error())
		}
	} else {
		s.Run.ApplyLocalConfig(merged)
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "tunnel": s.Run.View(merged)})
}

func (s *Server) startTunnel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	// Self-heal tunnels saved before the UUID was recorded.
	s.resolveAndPersist(id)
	if err := s.Run.Start(id); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) stopTunnel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	// Check existence first: the supervisor's record lookup creates an entry
	// for any id it is asked about, and nothing would ever reap it.
	if _, ok := s.Store.Tunnel(id); !ok {
		sendError(w, http.StatusNotFound, "隧道不存在")
		return
	}
	s.Run.Stop(id)
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) repairDNS(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	tunnel, ok := s.Store.Tunnel(id)
	if !ok {
		sendError(w, http.StatusNotFound, "隧道不存在")
		return
	}
	// The manual button bypasses the 10-minute cache: the user pressed it
	// because the route is wrong now.
	res := s.DNS.Ensure(tunnel)
	s.Run.SetDNSResult(id, res)

	if !res.OK {
		sendJSON(w, http.StatusBadRequest, dnsFailure{Result: res, Error: res.Message})
		return
	}
	sendJSON(w, http.StatusOK, res)
}

// dnsFailure is the body of a failed manual DNS repair: the same fields as a
// successful run, plus the "error" key.
//
// The frontend's request helper reports data.error for any non-2xx response and
// falls back to "请求失败 (400)" otherwise. dnsroute.Result has no such field, so
// every failed repair reached the user as that generic message and the reason
// it carried -- not logged in, tunnel UUID unknown, an API rejection -- was
// thrown away, which is exactly what the button exists to explain.
type dnsFailure struct {
	dnsroute.Result
	Error string `json:"error"`
}

func (s *Server) restartTunnel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	if _, ok := s.Store.Tunnel(id); !ok {
		sendError(w, http.StatusNotFound, "隧道不存在")
		return
	}
	s.Run.Stop(id)
	// Give the child time to release the socket before the replacement starts;
	// starting into a still-running process is the race this delay covers.
	time.AfterFunc(1200*time.Millisecond, func() { _ = s.Run.Start(id) })
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) tunnelLogs(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	if _, ok := s.Store.Tunnel(id); !ok {
		sendError(w, http.StatusNotFound, "隧道不存在")
		return
	}
	limit := 300
	if raw := r.URL.Query().Get("lines"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > paths.MaxLogLines {
		limit = paths.MaxLogLines
	}
	lines := s.Run.Logs(id, limit)
	if lines == nil {
		lines = []string{}
	}
	sendJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// probeOrigin dials the tunnel's known ingress origins on demand.
//
// This exists because "the tunnel is running" is not the same as "the service
// behind it answers": cloudflared reports a healthy tunnel even when every
// forwarded request gets a connection refused from the origin.
func (s *Server) probeOrigin(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	if _, ok := s.Store.Tunnel(id); !ok {
		sendError(w, http.StatusNotFound, "隧道不存在")
		return
	}
	origins := s.Run.ProbeOrigins(id)
	if origins == nil {
		origins = []runner.OriginStatus{}
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "origins": origins})
}

func (s *Server) getTunnel(w http.ResponseWriter, r *http.Request, id string) {
	tunnel, ok := s.Store.Tunnel(id)
	if !ok {
		sendError(w, http.StatusNotFound, "隧道不存在")
		return
	}
	sendJSON(w, http.StatusOK, s.Run.View(tunnel))
}

func (s *Server) updateTunnel(w http.ResponseWriter, r *http.Request, id string) {
	existing, ok := s.Store.Tunnel(id)
	if !ok {
		sendError(w, http.StatusNotFound, "隧道不存在")
		return
	}
	body, err := readBody(r)
	if err != nil {
		sendError(w, statusOf(err), err.Error())
		return
	}
	merged, err := config.MergeTunnel(&existing, body)
	if err != nil {
		sendError(w, statusOf(err), err.Error())
		return
	}
	if problems := config.ValidateTunnel(merged); len(problems) > 0 {
		sendError(w, http.StatusBadRequest, strings.Join(problems, "；"))
		return
	}
	merged.ID = existing.ID
	s.resolveLocalTunnel(&merged)

	wasRunning := s.Run.Running(id)
	s.Store.Replace(merged)
	if err := s.Store.Save(); err != nil {
		sendError(w, http.StatusInternalServerError, "保存隧道失败："+err.Error())
		return
	}
	if wasRunning {
		s.Run.Stop(id)
		time.AfterFunc(1200*time.Millisecond, func() { _ = s.Run.Start(id) })
	} else {
		// Saving while stopped must still take effect: the new hostnames have
		// to reach config.yml and DNS, or the UI reports success while nothing
		// changed.
		s.Run.ApplyLocalConfig(merged)
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "tunnel": s.Run.View(merged)})
}

func (s *Server) deleteTunnel(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := s.Store.Tunnel(id); !ok {
		sendError(w, http.StatusNotFound, "隧道不存在")
		return
	}
	s.Run.Stop(id)
	s.Store.Delete(id)
	if err := s.Store.Save(); err != nil {
		sendError(w, http.StatusInternalServerError, "保存隧道失败："+err.Error())
		return
	}
	s.Run.Forget(id)

	// Remove the tunnel's log and its rotated generation, so a deleted tunnel
	// does not leave up to 2MB of stale log behind forever.
	fsx.UnlinkQuiet(s.P.TunnelLog(id))
	fsx.UnlinkQuiet(s.P.TunnelLog(id) + ".1")
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resolveLocalTunnel records the UUID of a locally-managed tunnel that was
// picked by name.
//
// Running "whichever credentials file exists" silently attaches the wrong
// tunnel when the account has more than one, which looks exactly like a healthy
// tunnel serving the wrong origin. Resolving the name to a UUID once makes every
// later start and DNS call exact.
func (s *Server) resolveLocalTunnel(t *config.Tunnel) {
	if t == nil || t.Type != config.TypeLocal || t.TunnelID != "" || t.TunnelName == "" {
		return
	}
	resolved := s.Cf.ResolveTunnelIDByName(t.TunnelName)
	if resolved == "" {
		return
	}
	t.TunnelID = resolved
	s.Log.Info("resolved tunnel", t.TunnelName, "to", resolved)
}

// resolveAndPersist does the same for a tunnel already in the store.
func (s *Server) resolveAndPersist(id string) {
	tunnel, ok := s.Store.Tunnel(id)
	if !ok {
		return
	}
	before := tunnel.TunnelID
	s.resolveLocalTunnel(&tunnel)
	if tunnel.TunnelID == before {
		return
	}
	s.Store.Update(id, func(stored *config.Tunnel) { stored.TunnelID = tunnel.TunnelID })
	if err := s.Store.Save(); err != nil {
		s.Log.Warn("persisting resolved tunnel id:", err.Error())
	}
}
