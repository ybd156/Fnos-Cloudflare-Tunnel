package web

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

// handleAPI dispatches an /api/... request. The path shape is
// /api/<resource>/<id>/<action>, with an optional fourth segment used by the
// account tunnel routes.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request, apiPath string) {
	parts := apiSegments(apiPath)
	if len(parts) == 0 {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	resource := parts[0]
	var id, action, extra string
	if len(parts) > 1 {
		id = parts[1]
	}
	if len(parts) > 2 {
		action = parts[2]
	}
	if len(parts) > 3 {
		extra = parts[3]
	}

	switch resource {
	case "meta":
		s.handleMeta(w, r)
	case "settings":
		s.handleSettings(w, r)
	case "tunnels":
		s.handleTunnels(w, r, id, action)
	case "account":
		s.handleAccount(w, r, id, action, extra)
	case "logs":
		s.handleAppLog(w, r)
	case "binary":
		s.handleBinary(w, r, id)
	default:
		sendError(w, http.StatusNotFound, "接口不存在")
	}
}

// handleMeta reports the environment the UI displays in the header.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendError(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"app":     s.P.AppName,
		"version": s.Cf.AppVersion(),
		// The service used to be a Node program; it is a Go binary now, so the
		// UI shows the Go runtime instead of the Node version.
		"runtime":      runtime.Version(),
		"cloudflared":  s.Cf.Version(false),
		"binary":       s.P.ActiveCFBin(),
		"binaryExists": fsx.Exists(s.P.ActiveCFBin()),
		// Whether the binary in use is the one shipped in the package or one
		// updated in place. They live in different places because the payload is
		// replaced on every upgrade.
		"binarySource":  binarySource(s.P),
		"platform":      runtime.GOOS,
		"arch":          runtime.GOARCH,
		"gatewayPrefix": s.P.GatewayPrefix,
		"uptime":        int(time.Since(s.started).Seconds()),
		"loggedIn":      fsx.Exists(s.P.CertFile),
	})
}

// handleSettings reads and writes the global settings.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sendJSON(w, http.StatusOK, s.Store.Settings())
	case http.MethodPut:
		raw, err := readBodyRaw(r)
		if err != nil {
			sendError(w, statusOf(err), err.Error())
			return
		}
		body, err := decodeObject(raw)
		if err != nil {
			sendError(w, statusOf(err), err.Error())
			return
		}
		next, err := mergeSettings(s.Store.Settings(), body)
		if err != nil {
			sendError(w, statusOf(err), err.Error())
			return
		}
		// SetSettings marks them dirty so Save prefers memory over a
		// config.json that cmd/config_callback may have written meanwhile.
		s.Store.SetSettings(next)
		if err := s.Store.Save(); err != nil {
			sendError(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": s.Store.Settings()})
	default:
		sendError(w, http.StatusMethodNotAllowed, "方法不允许")
	}
}

// mergeSettings applies a partial settings payload over the current values and
// rejects anything out of range.
func mergeSettings(cur config.Settings, body map[string]json.RawMessage) (config.Settings, error) {
	next := cur

	for key, raw := range body {
		switch key {
		case "protocol":
			v, err := rawString(raw, key)
			if err != nil {
				return next, err
			}
			next.Protocol = v
		case "edgeIpVersion":
			// The old API accepted both 4 and "4"; keep tolerating that.
			v, err := rawString(raw, key)
			if err != nil {
				return next, err
			}
			next.EdgeIPVersion = v
		case "region":
			v, err := rawString(raw, key)
			if err != nil {
				return next, err
			}
			next.Region = v
		case "logLevel":
			v, err := rawString(raw, key)
			if err != nil {
				return next, err
			}
			next.LogLevel = v
		case "extraArgs":
			v, err := rawString(raw, key)
			if err != nil {
				return next, err
			}
			next.ExtraArgs = v
		case "metricsPort":
			v, err := rawInt(raw, key)
			if err != nil {
				return next, err
			}
			next.MetricsPort = v
		case "autoStart":
			v, err := rawBool(raw, key)
			if err != nil {
				return next, err
			}
			next.AutoStart = v
		case "autoRestart":
			v, err := rawBool(raw, key)
			if err != nil {
				return next, err
			}
			next.AutoRestart = v
		}
	}

	switch next.Protocol {
	case "auto", "http2", "quic":
	default:
		return next, badRequest("protocol 取值非法")
	}
	switch next.EdgeIPVersion {
	case "", "4", "6":
	default:
		return next, badRequest("edgeIpVersion 取值非法")
	}
	switch next.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return next, badRequest("logLevel 取值非法")
	}
	if next.MetricsPort < 0 || next.MetricsPort > 65535 {
		return next, badRequest("metrics 端口超出范围")
	}
	return next, nil
}

// handleAppLog returns the tail of the service log.
func (s *Server) handleAppLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendError(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}
	// Bounded tail read, matching the per-tunnel endpoint. Reading a 2MB window
	// on every 3-second poll was a measurable waste.
	lines := applog.ReadTailLines(s.P.AppLog, paths.MaxTailBytes, 300)
	if lines == nil {
		lines = []string{}
	}
	sendJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// handleBinary replaces the bundled cloudflared with the newest upstream build,
// or reverts to the bundled one.
func (s *Server) handleBinary(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	switch id {
	case "update":
		version, err := s.Cf.UpdateBinary()
		if err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
	case "reset":
		// Getting back to the bundled binary has to be possible: an update that
		// turns out badly would otherwise be permanent short of a reinstall.
		version, err := s.Cf.ResetBinary()
		if err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
	default:
		sendError(w, http.StatusNotFound, "接口不存在")
	}
}

// binarySource reports where the cloudflared in use came from.
func binarySource(p paths.Paths) string {
	if p.ActiveCFBin() == p.CFBinUpdated {
		return "updated"
	}
	return "bundled"
}

// rawString accepts a JSON string or number, matching the tolerance the
// previous implementation had for values coming from form controls.
func rawString(raw json.RawMessage, key string) (string, error) {
	if string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), nil
	}
	return "", badRequest("字段 " + key + " 取值非法")
}

func rawInt(raw json.RawMessage, key string) (int, error) {
	if string(raw) == "null" {
		return 0, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		// parseInt(...,10) || 0: a non-numeric value means zero.
		parsed, convErr := strconv.Atoi(s)
		if convErr != nil {
			return 0, nil
		}
		return parsed, nil
	}
	return 0, badRequest("字段 " + key + " 取值非法")
}

func rawBool(raw json.RawMessage, key string) (bool, error) {
	if string(raw) == "null" {
		return false, nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, badRequest("字段 " + key + " 取值非法")
	}
	return v, nil
}
