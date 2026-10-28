// Package paths derives every location the service uses, plus the tunables
// that bound its resource use, from the TRIM_* environment the fnOS app centre
// provides.
//
// Every location lives in one value (Paths) rather than in package-level
// variables so that tests can build a self-contained tree in a temp directory
// instead of depending on process-wide state set before the package loads.
package paths

import (
	"os"
	"path/filepath"
	"strconv"
)

// Resource bounds. Each one exists because the unbounded version was a real
// defect: an uncapped per-tunnel log filled the NAS system volume, and an
// unbounded tail read pulled the whole file into memory on every UI poll.
const (
	MaxLogLines       = 600       // ring buffer per tunnel in memory
	MaxBodyBytes      = 1 << 20   // largest accepted request body
	MaxAppLogBytes    = 2 << 20   // server.log rotation threshold
	MaxTunnelLogBytes = 1 << 20   // per-tunnel log rotation threshold
	MaxTailBytes      = 64 << 10  // window read by the log endpoints
	LogRotateCheckMS  = 30 * 1000 // how often rotation is considered
)

// Restart policy: exponential backoff, capped, and reset once a tunnel has
// been stable long enough that a later crash is a new problem rather than a
// continuation of a crash loop.
const (
	RestartBaseMS      = 5000
	RestartMaxMS       = 60000
	RestartMaxAttempts = 10
	RestartStableMS    = 120000
)

// Paths is the complete set of locations the service touches.
type Paths struct {
	AppName string
	AppDest string // installed payload, read-only, replaced on upgrade
	PkgVar  string // persistent data
	PkgTmp  string
	PkgEtc  string

	WWW           string
	CFBin         string // upstream cloudflared binary
	ServerBin     string // this service
	SocketPath    string
	GatewayPrefix string

	CFHome        string // HOME for the cloudflared child
	CFDir         string // cert.pem and <uuid>.json credentials
	CertFile      string
	TunnelsDir    string
	LogsDir       string
	ConfigFile    string
	AppLog        string
	CFVersionFile string
	PIDFile       string

	TCPHost string
	TCPPort int
}

// FromEnv builds Paths from the app centre environment, falling back to the
// standard /var/apps layout so the binary can be run by hand for debugging.
func FromEnv() Paths {
	appName := envOr("TRIM_APPNAME", "cloudflared")
	appDest := envOr("TRIM_APPDEST", filepath.Join("/var/apps", appName, "target"))
	pkgVar := envOr("TRIM_PKGVAR", filepath.Join("/var/apps", appName, "var"))
	pkgTmp := envOr("TRIM_PKGTMP", filepath.Join("/var/apps", appName, "tmp"))
	pkgEtc := envOr("TRIM_PKGETC", filepath.Join("/var/apps", appName, "etc"))

	cfDir := filepath.Join(pkgVar, ".cloudflared")
	port, _ := strconv.Atoi(envOr("TRIM_SERVICE_PORT", "0"))

	return Paths{
		AppName: appName,
		AppDest: appDest,
		PkgVar:  pkgVar,
		PkgTmp:  pkgTmp,
		PkgEtc:  pkgEtc,

		WWW:           filepath.Join(appDest, "www"),
		CFBin:         filepath.Join(appDest, "bin", "cloudflared"),
		ServerBin:     filepath.Join(appDest, "bin", "fncfd"),
		SocketPath:    filepath.Join(appDest, "app.sock"),
		GatewayPrefix: "/app/" + appName,

		CFHome:        pkgVar,
		CFDir:         cfDir,
		CertFile:      filepath.Join(cfDir, "cert.pem"),
		TunnelsDir:    filepath.Join(pkgVar, "tunnels"),
		LogsDir:       filepath.Join(pkgVar, "logs"),
		ConfigFile:    filepath.Join(pkgVar, "config.json"),
		AppLog:        filepath.Join(pkgVar, "server.log"),
		CFVersionFile: filepath.Join(pkgVar, "cloudflared.version"),
		PIDFile:       filepath.Join(pkgVar, "server.pid"),

		TCPHost: envOr("CF_TUNNEL_HOST", "127.0.0.1"),
		TCPPort: port,
	}
}

// Rooted returns Paths laid out under root using the same relative structure as
// a real install. Tests and the packaging lifecycle suite use it to build a
// self-contained tree without touching /var/apps or the process environment.
func Rooted(root string) Paths {
	p := FromEnv()
	p.AppDest = filepath.Join(root, "target")
	p.PkgVar = filepath.Join(root, "var")
	p.PkgTmp = filepath.Join(root, "tmp")
	p.PkgEtc = filepath.Join(root, "etc")

	p.WWW = filepath.Join(p.AppDest, "www")
	p.CFBin = filepath.Join(p.AppDest, "bin", "cloudflared")
	p.ServerBin = filepath.Join(p.AppDest, "bin", "fncfd")
	p.SocketPath = filepath.Join(p.AppDest, "app.sock")

	p.CFHome = p.PkgVar
	p.CFDir = filepath.Join(p.PkgVar, ".cloudflared")
	p.CertFile = filepath.Join(p.CFDir, "cert.pem")
	p.TunnelsDir = filepath.Join(p.PkgVar, "tunnels")
	p.LogsDir = filepath.Join(p.PkgVar, "logs")
	p.ConfigFile = filepath.Join(p.PkgVar, "config.json")
	p.AppLog = filepath.Join(p.PkgVar, "server.log")
	p.CFVersionFile = filepath.Join(p.PkgVar, "cloudflared.version")
	p.PIDFile = filepath.Join(p.PkgVar, "server.pid")
	p.TCPPort = 0
	return p
}

// EnsureDirs creates every directory the service writes to. It is the single
// entry point for directory creation: when it silently did nothing, saving a
// tunnel, writing config.yml, logging in and writing logs all failed after a
// fresh install, with no single place to see why.
func (p Paths) EnsureDirs() error {
	for _, dir := range []string{p.PkgVar, p.PkgTmp, p.CFDir, p.TunnelsDir, p.LogsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Credentials and cert.pem live here; keep the directory private.
	return os.Chmod(p.CFDir, 0o700)
}

// TunnelConfig is the generated config.yml for a locally-managed tunnel.
func (p Paths) TunnelConfig(id string) string {
	return filepath.Join(p.TunnelsDir, id, "config.yml")
}

// TunnelLog is the on-disk log for one tunnel.
func (p Paths) TunnelLog(id string) string {
	return filepath.Join(p.LogsDir, id+".log")
}

// CredentialFile is where `cloudflared tunnel create` writes a tunnel's
// credentials, named after the tunnel UUID.
func (p Paths) CredentialFile(uuid string) string {
	return filepath.Join(p.CFDir, uuid+".json")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
