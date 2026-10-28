// Package config owns the persisted state: config.json (global settings plus
// tunnel definitions), the validation of incoming tunnel payloads, and the
// merge rules that keep two writers from clobbering each other.
//
// The JSON field names are part of the on-disk contract: an installed app
// already has a config.json written by the previous implementation, so they
// must not change.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
)

// Tunnel types. Each maps to a different cloudflared invocation.
const (
	TypeToken  = "token"
	TypeQuick  = "quick"
	TypeLocal  = "local"
	TypeConfig = "config"
)

// Settings are the global options passed to every cloudflared command.
type Settings struct {
	Protocol      string `json:"protocol"`      // auto | http2 | quic
	EdgeIPVersion string `json:"edgeIpVersion"` // "" | "4" | "6"
	Region        string `json:"region"`
	LogLevel      string `json:"logLevel"` // debug | info | warn | error
	MetricsPort   int    `json:"metricsPort"`
	ExtraArgs     string `json:"extraArgs"`
	AutoStart     bool   `json:"autoStart"`
	AutoRestart   bool   `json:"autoRestart"`
}

// Ingress is one hostname -> local service rule.
type Ingress struct {
	Hostname         string `json:"hostname"`
	Path             string `json:"path"`
	Service          string `json:"service"`
	NoTLSVerify      bool   `json:"noTLSVerify"`
	OriginServerName string `json:"originServerName"`
	HTTPHostHeader   string `json:"httpHostHeader"`
}

// Tunnel is one supervised cloudflared process definition.
type Tunnel struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	Enabled         bool      `json:"enabled"`
	AutoStart       bool      `json:"autoStart"`
	AutoDNS         bool      `json:"autoDns"`
	Token           string    `json:"token"`
	Target          string    `json:"target"`
	QuickHostname   string    `json:"quickHostname"`
	TunnelName      string    `json:"tunnelName"`
	TunnelID        string    `json:"tunnelId"`
	CredentialsFile string    `json:"credentialsFile"`
	ConfigFile      string    `json:"configFile"`
	Ingress         []Ingress `json:"ingress"`
	CatchAll        string    `json:"catchAll"`
	ExtraArgs       string    `json:"extraArgs"`
	Note            string    `json:"note"`
}

// Config is the whole of config.json.
type Config struct {
	Version  int      `json:"version"`
	Settings Settings `json:"settings"`
	Tunnels  []Tunnel `json:"tunnels"`
}

// Patterns shared by validation and by the DNS and account layers.
var (
	// A tunnel id becomes a path segment (tunnels/<id>/config.yml and
	// logs/<id>.log), so a hand-edited config.json carrying ".." must not be
	// able to write outside those directories.
	SafeIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	UUIDRE   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// Only a hostname, optionally wildcarded: no scheme, port or path.
	DNSHostnameRE = regexp.MustCompile(`^(\*\.)?([A-Za-z0-9_-]+\.)+[A-Za-z]{2,}$`)
	// cloudflared's own tunnel-name rule.
	tunnelNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// DefaultSettings returns the settings a fresh install starts with.
func DefaultSettings() Settings {
	return Settings{
		Protocol:    "auto",
		LogLevel:    "info",
		MetricsPort: 0,
		AutoStart:   true,
		AutoRestart: true,
	}
}

// DefaultConfig is the empty state.
func DefaultConfig() Config {
	return Config{Version: 1, Settings: DefaultSettings(), Tunnels: []Tunnel{}}
}

// NewID returns a fresh tunnel id that is safe to use as a path segment.
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail in practice; a constant fallback keeps the
		// function total without inventing a failure path for callers.
		return "t_000000000000"
	}
	return "t_" + hex.EncodeToString(b[:])
}

// MaskSecret renders a credential for display: never the whole value.
func MaskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	r := []rune(secret)
	if len(r) <= 12 {
		return "••••••••"
	}
	return string(r[:6]) + "…" + string(r[len(r)-4:])
}

// ValidTunnelName reports whether name may be passed to `cloudflared tunnel
// create|delete|route`.
func ValidTunnelName(name string) bool { return tunnelNameRE.MatchString(name) }

// rawTunnel mirrors Tunnel but distinguishes an absent boolean from an
// explicitly false one, which is what makes "default to true unless the user
// turned it off" possible.
type rawTunnel struct {
	ID              *string   `json:"id"`
	Name            *string   `json:"name"`
	Type            *string   `json:"type"`
	Enabled         *bool     `json:"enabled"`
	AutoStart       *bool     `json:"autoStart"`
	AutoDNS         *bool     `json:"autoDns"`
	Token           *string   `json:"token"`
	Target          *string   `json:"target"`
	QuickHostname   *string   `json:"quickHostname"`
	TunnelName      *string   `json:"tunnelName"`
	TunnelID        *string   `json:"tunnelId"`
	CredentialsFile *string   `json:"credentialsFile"`
	ConfigFile      *string   `json:"configFile"`
	Ingress         []Ingress `json:"ingress"`
	CatchAll        *string   `json:"catchAll"`
	ExtraArgs       *string   `json:"extraArgs"`
	Note            *string   `json:"note"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func boolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

// NormalizeTunnel repairs whatever came off disk or off the wire into a Tunnel
// with every field in range. It never fails: an unusable id is replaced, an
// unknown type becomes a token tunnel.
func NormalizeTunnel(raw rawTunnel) Tunnel {
	id := str(raw.ID)
	if !SafeIDRE.MatchString(id) {
		id = NewID()
	}
	typ := str(raw.Type)
	switch typ {
	case TypeToken, TypeQuick, TypeLocal, TypeConfig:
	default:
		typ = TypeToken
	}
	name := strings.TrimSpace(str(raw.Name))
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	catchAll := str(raw.CatchAll)
	if catchAll == "" {
		catchAll = "http_status:404"
	}
	ingress := make([]Ingress, 0, len(raw.Ingress))
	for _, rule := range raw.Ingress {
		ingress = append(ingress, Ingress{
			Hostname:         rule.Hostname,
			Path:             rule.Path,
			Service:          rule.Service,
			NoTLSVerify:      rule.NoTLSVerify,
			OriginServerName: rule.OriginServerName,
			HTTPHostHeader:   rule.HTTPHostHeader,
		})
	}
	return Tunnel{
		ID:        id,
		Name:      name,
		Type:      typ,
		Enabled:   boolOr(raw.Enabled, true),
		AutoStart: boolOr(raw.AutoStart, true),
		// Default on: the easiest step to miss on a locally-managed tunnel is
		// the DNS route, and without it the hostname answers a Cloudflare edge
		// error while the tunnel itself looks healthy.
		AutoDNS:         boolOr(raw.AutoDNS, true),
		Token:           str(raw.Token),
		Target:          str(raw.Target),
		QuickHostname:   str(raw.QuickHostname),
		TunnelName:      str(raw.TunnelName),
		TunnelID:        str(raw.TunnelID),
		CredentialsFile: str(raw.CredentialsFile),
		ConfigFile:      str(raw.ConfigFile),
		Ingress:         ingress,
		CatchAll:        catchAll,
		ExtraArgs:       str(raw.ExtraArgs),
		Note:            str(raw.Note),
	}
}

// Clone returns a deep copy, so callers cannot mutate the stored state by
// accident.
func (t Tunnel) Clone() Tunnel {
	out := t
	out.Ingress = append([]Ingress(nil), t.Ingress...)
	return out
}

// Clone returns a deep copy of the whole configuration.
func (c Config) Clone() Config {
	out := c
	out.Tunnels = make([]Tunnel, len(c.Tunnels))
	for i, t := range c.Tunnels {
		out.Tunnels[i] = t.Clone()
	}
	return out
}

// MergeTunnel applies an API payload over an existing tunnel (nil when
// creating) and normalizes the result. Fields absent from the payload keep
// their existing value; that is what makes a partial edit safe.
//
// The token has two special cases carried over from the previous
// implementation: an empty string means "unchanged" (the UI never sees the
// token, so it cannot echo it back), and JSON null means "clear it".
func MergeTunnel(existing *Tunnel, body map[string]json.RawMessage) (Tunnel, error) {
	var raw rawTunnel
	if existing != nil {
		raw = toRaw(*existing)
	}
	for key, value := range body {
		if err := applyField(&raw, key, value); err != nil {
			return Tunnel{}, err
		}
	}
	merged := NormalizeTunnel(raw)
	if existing != nil {
		if b, ok := body["token"]; ok {
			if string(b) == `""` {
				merged.Token = existing.Token
			}
		}
	}
	return merged, nil
}

func toRaw(t Tunnel) rawTunnel {
	return rawTunnel{
		ID:              &t.ID,
		Name:            &t.Name,
		Type:            &t.Type,
		Enabled:         &t.Enabled,
		AutoStart:       &t.AutoStart,
		AutoDNS:         &t.AutoDNS,
		Token:           &t.Token,
		Target:          &t.Target,
		QuickHostname:   &t.QuickHostname,
		TunnelName:      &t.TunnelName,
		TunnelID:        &t.TunnelID,
		CredentialsFile: &t.CredentialsFile,
		ConfigFile:      &t.ConfigFile,
		Ingress:         t.Ingress,
		CatchAll:        &t.CatchAll,
		ExtraArgs:       &t.ExtraArgs,
		Note:            &t.Note,
	}
}

func applyField(raw *rawTunnel, key string, value json.RawMessage) error {
	// JSON null clears a field, matching the previous implementation where a
	// null token meant "remove the token".
	if string(value) == "null" {
		switch key {
		case "id":
			raw.ID = nil
		case "name":
			raw.Name = nil
		case "type":
			raw.Type = nil
		case "enabled":
			raw.Enabled = nil
		case "autoStart":
			raw.AutoStart = nil
		case "autoDns":
			raw.AutoDNS = nil
		case "token":
			empty := ""
			raw.Token = &empty
		case "target":
			raw.Target = nil
		case "quickHostname":
			raw.QuickHostname = nil
		case "tunnelName":
			raw.TunnelName = nil
		case "tunnelId":
			raw.TunnelID = nil
		case "credentialsFile":
			raw.CredentialsFile = nil
		case "configFile":
			raw.ConfigFile = nil
		case "ingress":
			raw.Ingress = nil
		case "catchAll":
			raw.CatchAll = nil
		case "extraArgs":
			raw.ExtraArgs = nil
		case "note":
			raw.Note = nil
		}
		return nil
	}

	dec := func(dst any) error {
		if err := json.Unmarshal(value, dst); err != nil {
			return fmt.Errorf("字段 %s 取值非法", key)
		}
		return nil
	}
	switch key {
	case "id":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.ID = &v
	case "name":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.Name = &v
	case "type":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.Type = &v
	case "enabled":
		var v bool
		if err := dec(&v); err != nil {
			return err
		}
		raw.Enabled = &v
	case "autoStart":
		var v bool
		if err := dec(&v); err != nil {
			return err
		}
		raw.AutoStart = &v
	case "autoDns":
		var v bool
		if err := dec(&v); err != nil {
			return err
		}
		raw.AutoDNS = &v
	case "token":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.Token = &v
	case "target":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.Target = &v
	case "quickHostname":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.QuickHostname = &v
	case "tunnelName":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.TunnelName = &v
	case "tunnelId":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.TunnelID = &v
	case "credentialsFile":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.CredentialsFile = &v
	case "configFile":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.ConfigFile = &v
	case "ingress":
		var v []Ingress
		if err := dec(&v); err != nil {
			return err
		}
		raw.Ingress = v
	case "catchAll":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.CatchAll = &v
	case "extraArgs":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.ExtraArgs = &v
	case "note":
		var v string
		if err := dec(&v); err != nil {
			return err
		}
		raw.Note = &v
	default:
		// Unknown keys are ignored: a newer UI must not break an older server.
	}
	return nil
}

// ValidateTunnel reports every reason a normalized tunnel cannot be run. The
// messages are shown verbatim in the UI.
func ValidateTunnel(t Tunnel) []string {
	var errors []string
	if strings.TrimSpace(t.Name) == "" {
		errors = append(errors, "请填写名称")
	}
	if t.Type == TypeToken && t.Token == "" {
		errors = append(errors, "请填写隧道 Token")
	}
	if t.Type == TypeQuick && !hasHTTPScheme(t.Target) {
		errors = append(errors, "本地服务地址需以 http:// 或 https:// 开头")
	}
	if t.Type == TypeLocal && t.TunnelName == "" && t.TunnelID == "" {
		errors = append(errors, "请选择要运行的隧道")
	}
	if t.Type == TypeConfig && t.ConfigFile == "" {
		errors = append(errors, "请填写配置文件路径")
	}
	if strings.Contains(t.ConfigFile, "..") {
		errors = append(errors, "配置文件路径非法")
	}
	for _, rule := range t.Ingress {
		if rule.Hostname == "" {
			errors = append(errors, "入站规则缺少域名")
		} else if !DNSHostnameRE.MatchString(rule.Hostname) {
			errors = append(errors, fmt.Sprintf(
				"入站规则域名不合法：%s（只填主机名，例如 nas.example.com，不要带协议、端口或路径）", rule.Hostname))
		}
		if !hasHTTPScheme(rule.Service) {
			errors = append(errors, fmt.Sprintf("入站规则 %s 的服务地址需以 http:// 或 https:// 开头", rule.Hostname))
		}
	}
	return errors
}

func hasHTTPScheme(v string) bool {
	lower := strings.ToLower(v)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// Store is the in-memory configuration plus the file it is persisted to. All
// access is guarded: unlike the single-threaded previous implementation, HTTP
// handlers and the process supervisor touch this concurrently.
type Store struct {
	path string
	log  *applog.Logger

	mu  sync.RWMutex
	cfg Config
	// settingsDirty records whether THIS process changed settings since the
	// last load or save. saveLocked uses it to tell "I changed this" from
	// "another process did".
	settingsDirty bool
}

// NewStore returns a store backed by path. Call Load before use.
func NewStore(path string, log *applog.Logger) *Store {
	return &Store{path: path, log: log, cfg: DefaultConfig()}
}

// Load reads config.json, falling back to defaults. An unreadable file is
// logged rather than fatal: a corrupt config must not stop the service, or the
// UI that could repair it would be down too.
func (s *Store) Load() {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log.Warn("config.json unreadable, starting fresh:", err.Error())
		}
		s.cfg = DefaultConfig()
		s.settingsDirty = false
		return
	}
	// Settings start from the defaults and are overwritten key by key, so a
	// config.json written by an older version keeps working.
	cfg := DefaultConfig()
	var file struct {
		Version  int             `json:"version"`
		Settings json.RawMessage `json:"settings"`
		Tunnels  []rawTunnel     `json:"tunnels"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		s.log.Warn("config.json unreadable, starting fresh:", err.Error())
		s.cfg = DefaultConfig()
		s.settingsDirty = false
		return
	}
	// Decode the settings *object*, not the whole document. Unmarshalling the
	// document into Settings matches no key at the top level and therefore
	// silently leaves every default in place -- which looks exactly like
	// "my settings reset after every restart".
	if len(file.Settings) > 0 {
		if err := json.Unmarshal(file.Settings, &cfg.Settings); err != nil {
			s.log.Warn("config.json settings unreadable, using defaults:", err.Error())
			cfg.Settings = DefaultSettings()
		}
	}
	tunnels := make([]Tunnel, 0, len(file.Tunnels))
	for _, rt := range file.Tunnels {
		tunnels = append(tunnels, NormalizeTunnel(rt))
	}
	cfg.Version = 1
	cfg.Tunnels = tunnels
	s.cfg = cfg
	s.settingsDirty = false
}

// Save persists the configuration.
//
// The App Center settings page runs cmd/config_callback, a separate short-lived
// process that edits the same file while this service holds its own copy in
// memory. Writing the in-memory object wholesale would silently revert that
// change -- both pages report success and the user only notices the setting
// drifted back. So the two halves have different owners:
//
//	settings -- writable from either side, so the newer writer wins.
//	tunnels  -- only ever written here, so memory is authoritative.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	merged := s.cfg
	if !s.settingsDirty {
		if raw, err := os.ReadFile(s.path); err == nil {
			var disk struct {
				Settings json.RawMessage `json:"settings"`
			}
			if json.Unmarshal(raw, &disk) == nil && len(disk.Settings) > 0 {
				settings := DefaultSettings()
				if json.Unmarshal(disk.Settings, &settings) == nil {
					merged.Settings = settings
				}
			}
		}
	}
	merged.Version = 1
	if merged.Tunnels == nil {
		merged.Tunnels = []Tunnel{}
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	s.cfg = merged
	s.settingsDirty = false
	return fsx.AtomicWrite(s.path, data, 0o600)
}

// Snapshot returns a deep copy of the whole configuration.
func (s *Store) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// Settings returns the current global settings.
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Settings
}

// SetSettings replaces the global settings and marks them as changed here, so
// the next Save() prefers memory over whatever is on disk.
func (s *Store) SetSettings(next Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Settings = next
	s.settingsDirty = true
}

// Tunnels returns a copy of every tunnel definition.
func (s *Store) Tunnels() []Tunnel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Tunnel, len(s.cfg.Tunnels))
	for i, t := range s.cfg.Tunnels {
		out[i] = t.Clone()
	}
	return out
}

// Tunnel looks one tunnel up by id.
func (s *Store) Tunnel(id string) (Tunnel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.cfg.Tunnels {
		if t.ID == id {
			return t.Clone(), true
		}
	}
	return Tunnel{}, false
}

// Add appends a tunnel definition.
func (s *Store) Add(t Tunnel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Tunnels = append(s.cfg.Tunnels, t.Clone())
}

// Replace swaps the definition with matching id. It reports whether the tunnel
// existed.
func (s *Store) Replace(t Tunnel) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.cfg.Tunnels {
		if existing.ID == t.ID {
			s.cfg.Tunnels[i] = t.Clone()
			return true
		}
	}
	return false
}

// Update mutates one tunnel in place under the store lock. Used for the
// self-healing writes (recording a resolved tunnel UUID).
func (s *Store) Update(id string, fn func(*Tunnel)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Tunnels {
		if s.cfg.Tunnels[i].ID == id {
			fn(&s.cfg.Tunnels[i])
			return true
		}
	}
	return false
}

// Delete removes a tunnel definition and reports whether it existed.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.cfg.Tunnels {
		if t.ID == id {
			s.cfg.Tunnels = append(s.cfg.Tunnels[:i], s.cfg.Tunnels[i+1:]...)
			return true
		}
	}
	return false
}
