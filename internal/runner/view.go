package runner

import (
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/dnsroute"
)

// DNSResults is the outcome of one DNS route check. The field names are the
// ones the UI already reads from the previous implementation's record.
type DNSResults struct {
	Routed    []string           `json:"routed"`
	AlreadyOK []string           `json:"alreadyOk"`
	Failed    []dnsroute.Failure `json:"failed"`
}

// View is one tunnel as the API returns it: the stored definition plus the
// runtime record. The JSON names are the contract with the front end and must
// stay identical to the previous tunnelView object.
//
// The nullable runtime fields are pointers because the previous implementation
// emitted null for them: the UI distinguishes "never started" from "started at
// the epoch".
type View struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Type            string           `json:"type"`
	Enabled         bool             `json:"enabled"`
	AutoStart       bool             `json:"autoStart"`
	AutoDNS         bool             `json:"autoDns"`
	Target          string           `json:"target"`
	QuickHostname   string           `json:"quickHostname"`
	TunnelName      string           `json:"tunnelName"`
	TunnelID        string           `json:"tunnelId"`
	CredentialsFile string           `json:"credentialsFile"`
	ConfigFile      string           `json:"configFile"`
	Ingress         []config.Ingress `json:"ingress"`
	CatchAll        string           `json:"catchAll"`
	ExtraArgs       string           `json:"extraArgs"`
	Note            string           `json:"note"`
	HasToken        bool             `json:"hasToken"`
	TokenPreview    string           `json:"tokenPreview"`
	Status          string           `json:"status"`
	PID             *int             `json:"pid"`
	URL             string           `json:"url"`
	Error           string           `json:"error"`
	StartedAt       *string          `json:"startedAt"`
	StoppedAt       *string          `json:"stoppedAt"`
	LastExitCode    *int             `json:"lastExitCode"`
	LastExitAt      *string          `json:"lastExitAt"`
	Attempts        int              `json:"attempts"`
	DNSStatus       string           `json:"dnsStatus"`
	DNSResults      *DNSResults      `json:"dnsResults"`
	DNSMessage      string           `json:"dnsMessage"`
	DNSRunning      bool             `json:"dnsRunning"`

	// Origin health. Both are always present so the UI can tell "no problem"
	// from "this field does not exist yet" without guessing.
	OriginError *OriginError   `json:"originError"`
	Origins     []OriginStatus `json:"origins"`

	// Transfer failures in the recent log window. Zero values mean "nothing
	// seen", which is what a healthy tunnel should report.
	Transfer TransferStats `json:"transfer"`
}

// View renders one tunnel for the API. It creates the runtime record if the
// tunnel has never been started, so a fresh tunnel reports "stopped" rather
// than being missing.
func (m *Manager) View(t config.Tunnel) View {
	m.mu.Lock()
	r := m.recordLocked(t.ID)

	v := View{
		ID:              t.ID,
		Name:            t.Name,
		Type:            t.Type,
		Enabled:         t.Enabled,
		AutoStart:       t.AutoStart,
		AutoDNS:         t.AutoDNS,
		Target:          t.Target,
		QuickHostname:   t.QuickHostname,
		TunnelName:      t.TunnelName,
		TunnelID:        t.TunnelID,
		CredentialsFile: t.CredentialsFile,
		ConfigFile:      t.ConfigFile,
		Ingress:         ingressCopy(t.Ingress),
		CatchAll:        t.CatchAll,
		ExtraArgs:       t.ExtraArgs,
		Note:            t.Note,
		HasToken:        t.Token != "",
		TokenPreview:    config.MaskSecret(t.Token),

		Status:     r.status,
		URL:        r.url,
		Error:      r.errMsg,
		StartedAt:  stringPtr(r.startedAt),
		StoppedAt:  stringPtr(r.stoppedAt),
		LastExitAt: stringPtr(r.lastExitAt),
		Attempts:   r.attempts,

		DNSStatus:  r.dnsStatus,
		DNSMessage: r.dnsMessage,
		DNSRunning: r.dnsRunning,

		Origins: originCopy(r.origins),
	}
	// Computed under the lock: the ring is being appended to by the log pump.
	v.Transfer = ScanTransfers(r.log.tail(transferWindow))
	if r.originErr != nil {
		// Copy: the record may be overwritten by the next log line while the
		// response is being serialised.
		e := *r.originErr
		v.OriginError = &e
	}
	if r.pid > 0 {
		pid := r.pid
		v.PID = &pid
	}
	if r.lastExitCode != nil {
		code := *r.lastExitCode
		v.LastExitCode = &code
	}
	if r.dnsResults != nil {
		v.DNSResults = copyResults(r.dnsResults)
	}
	m.mu.Unlock()

	return v
}

// Running reports whether the tunnel has a live child process. The API needs it
// to decide whether a save has to restart the tunnel; it replaces reading the
// runtime record directly, which the HTTP layer must not do.
func (m *Manager) Running(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return false
	}
	return r.child != nil
}

// originCopy returns the probe results, always as a non-nil slice: the UI reads
// .length and a null would need special-casing.
func originCopy(in []OriginStatus) []OriginStatus {
	if len(in) == 0 {
		return []OriginStatus{}
	}
	return append([]OriginStatus(nil), in...)
}

func ingressCopy(in []config.Ingress) []config.Ingress {
	if len(in) == 0 {
		// The previous implementation always emitted an array here.
		return []config.Ingress{}
	}
	return append([]config.Ingress(nil), in...)
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	out := s
	return &out
}

func copyResults(in *DNSResults) *DNSResults {
	return &DNSResults{
		Routed:    nonNilStrings(in.Routed),
		AlreadyOK: nonNilStrings(in.AlreadyOK),
		Failed:    nonNilFailures(in.Failed),
	}
}
