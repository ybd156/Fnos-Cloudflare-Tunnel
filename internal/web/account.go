package web

import (
	"net/http"
)

// handleAccount serves the Cloudflare account surface: login/logout, the list of
// named tunnels on the account, and creating, deleting and routing them.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request, id, action, extra string) {
	if id == "" {
		if r.Method != http.MethodGet {
			sendError(w, http.StatusNotFound, "接口不存在")
			return
		}
		sendJSON(w, http.StatusOK, s.Cf.LoginStatus())
		return
	}

	switch id {
	case "login":
		switch r.Method {
		case http.MethodGet:
			sendJSON(w, http.StatusOK, s.Cf.LoginStatus())
		case http.MethodPost:
			if err := s.Cf.LoginStart(); err != nil {
				sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			status := s.Cf.LoginStatus()
			sendJSON(w, http.StatusOK, map[string]any{"ok": true, "url": status.URL, "status": status.Status})
		default:
			sendError(w, http.StatusNotFound, "接口不存在")
		}

	case "logout":
		if r.Method != http.MethodPost {
			sendError(w, http.StatusNotFound, "接口不存在")
			return
		}
		s.Cf.Logout()
		sendJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "tunnels":
		s.handleAccountTunnels(w, r, action, extra)

	default:
		sendError(w, http.StatusNotFound, "接口不存在")
	}
}

func (s *Server) handleAccountTunnels(w http.ResponseWriter, r *http.Request, name, extra string) {
	// POST /api/account/tunnels/<name>/dns -- create the CNAME route for a
	// hostname on a named tunnel.
	if name != "" && extra == "dns" {
		if r.Method != http.MethodPost {
			sendError(w, http.StatusNotFound, "接口不存在")
			return
		}
		body, err := readBody(r)
		if err != nil {
			sendError(w, statusOf(err), err.Error())
			return
		}
		if err := s.Cf.RouteDNS(name, bodyString(body, "hostname")); err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if name != "" && r.Method == http.MethodDelete {
		if err := s.Cf.DeleteTunnel(name); err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if name != "" {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}

	switch r.Method {
	case http.MethodGet:
		tunnels, err := s.Cf.ListTunnels()
		if err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error(), "tunnels": []any{}})
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"ok": true, "tunnels": tunnels})

	case http.MethodPost:
		body, err := readBody(r)
		if err != nil {
			sendError(w, statusOf(err), err.Error())
			return
		}
		raw, output, err := s.Cf.CreateTunnel(bodyString(body, "name"))
		if err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"ok": true, "tunnel": raw, "output": output})

	default:
		sendError(w, http.StatusNotFound, "接口不存在")
	}
}
