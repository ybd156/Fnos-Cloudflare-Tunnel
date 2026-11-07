package web

import (
	"net/http"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/cf"
)

// handleDiagnose runs the network self-test the account page offers: the hosts
// cloudflared must resolve, plus a TCP dial to the API.
//
// It is read-only and takes no input. A NAS whose gateway refuses DNS queries
// cannot log in and cannot use the API; without this the only symptom was
// "exit code 1" from cloudflared.
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendError(w, http.StatusNotFound, "接口不存在")
		return
	}
	sendJSON(w, http.StatusOK, cf.Diagnose(5*time.Second))
}
