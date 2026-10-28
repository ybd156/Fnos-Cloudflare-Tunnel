package cf

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ybd156/fnos-cloudflared/internal/applog"
	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

const testUUID = "0f5f1a2b-3c4d-5e6f-7a8b-9c0d1e2f3a4b"

// testPaths builds a self-contained tree so no test touches the real install.
func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "apps", "cloudflared")
	p := paths.Paths{
		AppName: "cloudflared",
		AppDest: filepath.Join(base, "target"),
		PkgVar:  filepath.Join(base, "var"),
		PkgTmp:  filepath.Join(base, "tmp"),
		PkgEtc:  filepath.Join(base, "etc"),
	}
	p.CFBin = filepath.Join(p.AppDest, "bin", "cloudflared")
	p.CFDir = filepath.Join(p.PkgVar, ".cloudflared")
	p.CFHome = p.PkgVar
	p.CertFile = filepath.Join(p.CFDir, "cert.pem")
	p.TunnelsDir = filepath.Join(p.PkgVar, "tunnels")
	p.LogsDir = filepath.Join(p.PkgVar, "logs")
	p.ConfigFile = filepath.Join(p.PkgVar, "config.json")
	p.AppLog = filepath.Join(p.PkgVar, "server.log")
	p.CFVersionFile = filepath.Join(p.PkgVar, "cloudflared.version")
	return p
}

func newTestRunner(t *testing.T) (*Runner, paths.Paths) {
	t.Helper()
	p := testPaths(t)
	fsx.MkdirAll(p.PkgVar, 0o755)
	log := applog.New(p.AppLog)
	log.Out = io.Discard
	log.Err = io.Discard
	t.Cleanup(log.Close)
	return New(p, log), p
}

// writeCert creates the login artefact every account call requires.
func writeCert(t *testing.T, p paths.Paths) {
	t.Helper()
	fsx.MkdirAll(p.CFDir, 0o700)
	if err := os.WriteFile(p.CertFile, []byte("cert"), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
}

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"   ", []string{}},
		{"a b  c", []string{"a", "b", "c"}},
		{`--edge-ip-version 4 --region us`, []string{"--edge-ip-version", "4", "--region", "us"}},
		{`"a b" c`, []string{"a b", "c"}},
		{`'a b' c`, []string{"a b", "c"}},
		{`""`, []string{""}},
		{`--hostname="a.example.com"`, []string{`--hostname="a.example.com"`}},
		{`"unterminated`, []string{`"unterminated`}},
		{`a"b c"d`, []string{`a"b`, `c"d`}},
		{"--x\t1\n--y 2", []string{"--x", "1", "--y", "2"}},
	}
	for _, c := range cases {
		got := SplitArgs(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitArgs(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestGlobalArgs(t *testing.T) {
	// Defaults: protocol "auto" is omitted, the default log level and
	// --no-autoupdate are always present.
	got := GlobalArgs(config.DefaultSettings())
	want := []string{"--loglevel", "info", "--no-autoupdate"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %#v, want %#v", got, want)
	}

	s := config.Settings{
		Protocol:      "quic",
		EdgeIPVersion: "4",
		Region:        "us",
		LogLevel:      "debug",
		MetricsPort:   20241,
		ExtraArgs:     `--retries 3 --grace-period "10s"`,
	}
	want = []string{
		"--protocol", "quic",
		"--edge-ip-version", "4",
		"--region", "us",
		"--loglevel", "debug",
		"--metrics", "127.0.0.1:20241",
		"--no-autoupdate",
		"--retries", "3", "--grace-period", "10s",
	}
	if got := GlobalArgs(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("GlobalArgs = %#v, want %#v", got, want)
	}
}

func TestYamlScalar(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", `""`},
		{"abc", "abc"},
		{"nas.example.com", "nas.example.com"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http_status:404", "http_status:404"},
		{"/var/apps/cloudflared/var/.cloudflared/x.json", "/var/apps/cloudflared/var/.cloudflared/x.json"},
		// Values YAML would read as a boolean or null must be quoted.
		{"true", `"true"`},
		{"false", `"false"`},
		{"Yes", `"Yes"`},
		{"OFF", `"OFF"`},
		{"null", `"null"`},
		{"~", `"~"`},
		// Everything else takes JSON double-quoted style.
		{"a b", `"a b"`},
		{"a: b", `"a: b"`},
		{"a#b", `"a#b"`},
		{`a"b`, `"a\"b"`},
		{"a\\b", `"a\\b"`},
		{"中文", `"中文"`},
		// JSON.stringify left these literal; SetEscapeHTML(false) matches it.
		{"a<b>&c", `"a<b>&c"`},
	}
	for _, c := range cases {
		if got := yamlScalar(c.in); got != c.want {
			t.Errorf("yamlScalar(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestBuildConfigYAML(t *testing.T) {
	tun := config.Tunnel{
		ID:         "t_abc",
		Type:       config.TypeLocal,
		TunnelName: "my-tunnel",
		TunnelID:   testUUID,
		Ingress: []config.Ingress{
			{
				Hostname:         "nas.example.com",
				Path:             "/api",
				Service:          "http://127.0.0.1:5000",
				NoTLSVerify:      true,
				OriginServerName: "origin.example.com",
				HTTPHostHeader:   "localhost",
			},
			{Hostname: "empty-svc.example.com"},
			{Hostname: "", Service: "http://127.0.0.1:9"}, // no hostname: skipped
		},
		CatchAll: "http_status:404",
	}
	cred := "/var/apps/cloudflared/var/.cloudflared/" + testUUID + ".json"

	want := strings.Join([]string{
		"# Generated by Cloudflare Tunnel for fnOS. Manual edits are overwritten.",
		"tunnel: " + testUUID,
		"credentials-file: " + cred,
		"ingress:",
		"  - hostname: nas.example.com",
		"    path: /api",
		"    service: http://127.0.0.1:5000",
		"    originRequest:",
		`      noTLSVerify: "true"`,
		"      originServerName: origin.example.com",
		"      httpHostHeader: localhost",
		"  - hostname: empty-svc.example.com",
		"    service: http://127.0.0.1:80",
		"  - service: http_status:404",
		"",
	}, "\n")

	if got := BuildConfigYAML(tun, cred); got != want {
		t.Fatalf("BuildConfigYAML mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildConfigYAMLDefaults(t *testing.T) {
	tun := config.Tunnel{ID: "t_1", Type: config.TypeLocal, TunnelName: "by-name"}
	got := BuildConfigYAML(tun, "")

	// The tunnel reference falls back to the name when the UUID is unknown.
	if !strings.Contains(got, "\ntunnel: by-name\n") {
		t.Errorf("missing tunnel name reference:\n%s", got)
	}
	if strings.Contains(got, "credentials-file:") {
		t.Errorf("credentials-file must be omitted when empty:\n%s", got)
	}
	// A config without a catch-all is rejected by cloudflared.
	if !strings.HasSuffix(got, "  - service: http_status:404\n") {
		t.Errorf("missing catch-all:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("output must end with a newline")
	}
}

func TestResolveCredentials(t *testing.T) {
	r, p := newTestRunner(t)

	// 1. An explicit path wins.
	explicit := filepath.Join(t.TempDir(), "chosen.json")
	if err := os.WriteFile(explicit, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := r.ResolveCredentials(config.Tunnel{CredentialsFile: explicit}); got != explicit {
		t.Fatalf("explicit path = %q, want %q", got, explicit)
	}

	// 2. An explicit path that does not exist never falls through to a guess.
	fsx.MkdirAll(p.CFDir, 0o700)
	uuidFile := p.CredentialFile(testUUID)
	if err := os.WriteFile(uuidFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "gone.json")
	if got := r.ResolveCredentials(config.Tunnel{
		CredentialsFile: missing,
		TunnelID:        testUUID,
	}); got != "" {
		t.Fatalf("missing explicit path resolved to %q, want \"\"", got)
	}

	// 3. <uuid>.json is provable.
	if got := r.ResolveCredentials(config.Tunnel{TunnelID: testUUID}); got != uuidFile {
		t.Fatalf("uuid credentials = %q, want %q", got, uuidFile)
	}

	// 4. Uppercase UUIDs still resolve (the previous pattern was
	// case-insensitive) and keep the stored spelling in the path.
	upper := strings.ToUpper(testUUID)
	upperFile := p.CredentialFile(upper)
	if err := os.WriteFile(upperFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := r.ResolveCredentials(config.Tunnel{TunnelID: upper}); got != upperFile {
		t.Fatalf("uppercase uuid credentials = %q, want %q", got, upperFile)
	}

	// 5. A name-only tunnel must not pick "some .json in the directory".
	if got := r.ResolveCredentials(config.Tunnel{TunnelName: "my-tunnel"}); got != "" {
		t.Fatalf("name-only tunnel guessed %q, want \"\"", got)
	}
	// 6. A non-UUID tunnel id is not provable either.
	if got := r.ResolveCredentials(config.Tunnel{TunnelID: "not-a-uuid"}); got != "" {
		t.Fatalf("non-uuid id guessed %q, want \"\"", got)
	}
}

func TestBuildArgsToken(t *testing.T) {
	r, _ := newTestRunner(t)
	s := config.DefaultSettings()

	args, env, err := r.BuildArgs(config.Tunnel{Type: config.TypeToken, Token: "super-secret"}, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"--loglevel", "info", "--no-autoupdate", "tunnel", "run"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	if env["TUNNEL_TOKEN"] != "super-secret" {
		t.Fatalf("env = %#v, want TUNNEL_TOKEN", env)
	}
	// The token must never reach the command line.
	for _, a := range args {
		if strings.Contains(a, "super-secret") {
			t.Fatalf("token leaked into argv: %#v", args)
		}
	}

	if _, _, err := r.BuildArgs(config.Tunnel{Type: config.TypeToken}, s); err == nil ||
		err.Error() != "未配置隧道 Token" {
		t.Fatalf("missing token error = %v", err)
	}
}

func TestBuildArgsQuick(t *testing.T) {
	r, _ := newTestRunner(t)
	s := config.DefaultSettings()

	args, _, err := r.BuildArgs(config.Tunnel{
		Type:          config.TypeQuick,
		Target:        "http://127.0.0.1:8080",
		QuickHostname: "abc.example.com",
	}, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"--loglevel", "info", "--no-autoupdate", "tunnel",
		"--url", "http://127.0.0.1:8080", "--hostname", "abc.example.com"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}

	if _, _, err := r.BuildArgs(config.Tunnel{Type: config.TypeQuick}, s); err == nil ||
		err.Error() != "未配置本地服务地址" {
		t.Fatalf("missing target error = %v", err)
	}
}

func TestBuildArgsLocal(t *testing.T) {
	r, p := newTestRunner(t)
	s := config.DefaultSettings()
	fsx.MkdirAll(p.CFDir, 0o700)
	if err := os.WriteFile(p.CredentialFile(testUUID), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	tun := config.Tunnel{
		ID:         "t_local",
		Type:       config.TypeLocal,
		TunnelName: "my-tunnel",
		TunnelID:   testUUID,
		Ingress:    []config.Ingress{{Hostname: "nas.example.com", Service: "http://127.0.0.1:5000"}},
		ExtraArgs:  "--loglevel debug",
	}
	args, env, err := r.BuildArgs(tun, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("local tunnel env = %#v, want empty", env)
	}

	configPath := p.TunnelConfig("t_local")
	want := []string{"--loglevel", "info", "--no-autoupdate", "--config", configPath,
		"tunnel", "run", "my-tunnel", "--loglevel", "debug"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	// The config file is written by BuildArgs so a restart always uses the
	// current definition.
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config.yml not written: %v", err)
	}
	if !strings.Contains(string(data), "hostname: nas.example.com") {
		t.Fatalf("config.yml missing ingress rule:\n%s", data)
	}

	// Neither a name nor an id: nothing to run.
	if _, _, err := r.BuildArgs(config.Tunnel{Type: config.TypeLocal}, s); err == nil ||
		err.Error() != "未选择已创建的隧道" {
		t.Fatalf("no tunnel error = %v", err)
	}

	// A known UUID without its credentials file: say exactly which file.
	otherUUID := "11111111-2222-3333-4444-555555555555"
	_, _, err = r.BuildArgs(config.Tunnel{ID: "t2", Type: config.TypeLocal, TunnelID: otherUUID}, s)
	wantMsg := "找不到隧道凭据文件 " + p.CredentialFile(otherUUID) +
		"，请先在“账户”页重新创建或重新选择该隧道"
	if err == nil || err.Error() != wantMsg {
		t.Fatalf("missing credentials error = %v, want %q", err, wantMsg)
	}

	// A name-only tunnel asks for a re-selection instead of guessing.
	_, _, err = r.BuildArgs(config.Tunnel{ID: "t3", Type: config.TypeLocal, TunnelName: "lonely"}, s)
	wantMsg = "无法确定隧道「lonely」的 UUID，请在“账户”页重新选择该隧道（需要已登录账户）"
	if err == nil || err.Error() != wantMsg {
		t.Fatalf("unresolved uuid error = %v, want %q", err, wantMsg)
	}
}

func TestBuildArgsConfigFile(t *testing.T) {
	r, _ := newTestRunner(t)
	s := config.DefaultSettings()

	if _, _, err := r.BuildArgs(config.Tunnel{Type: config.TypeConfig}, s); err == nil ||
		err.Error() != "未指定配置文件路径" {
		t.Fatalf("empty path error = %v", err)
	}

	missing := filepath.Join(t.TempDir(), "nope.yml")
	_, _, err := r.BuildArgs(config.Tunnel{Type: config.TypeConfig, ConfigFile: missing}, s)
	if err == nil || err.Error() != "配置文件不存在: "+missing {
		t.Fatalf("missing file error = %v", err)
	}

	existing := filepath.Join(t.TempDir(), "cfg.yml")
	if werr := os.WriteFile(existing, []byte("ingress: []\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	args, _, err := r.BuildArgs(config.Tunnel{Type: config.TypeConfig, ConfigFile: existing}, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"--loglevel", "info", "--no-autoupdate", "--config", existing, "tunnel", "run"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}

	if _, _, err := r.BuildArgs(config.Tunnel{Type: "bogus"}, s); err == nil ||
		err.Error() != "未知的隧道类型: bogus" {
		t.Fatalf("unknown type error = %v", err)
	}
}

func TestEnvOverrides(t *testing.T) {
	r, p := newTestRunner(t)

	env := r.Env(map[string]string{"TUNNEL_TOKEN": "tok"})
	got := map[string]string{}
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			t.Fatalf("malformed env entry %q", kv)
		}
		k := kv[:i]
		if _, dup := got[k]; dup {
			t.Fatalf("duplicate env key %q", k)
		}
		got[k] = kv[i+1:]
	}

	if got["HOME"] != p.CFHome {
		t.Errorf("HOME = %q, want %q", got["HOME"], p.CFHome)
	}
	if got["TUNNEL_ORIGIN_CERT"] != p.CertFile {
		t.Errorf("TUNNEL_ORIGIN_CERT = %q, want %q", got["TUNNEL_ORIGIN_CERT"], p.CertFile)
	}
	if got["NO_AUTOUPDATE"] != "true" {
		t.Errorf("NO_AUTOUPDATE = %q, want true", got["NO_AUTOUPDATE"])
	}
	if got["TUNNEL_TOKEN"] != "tok" {
		t.Errorf("TUNNEL_TOKEN = %q, want tok", got["TUNNEL_TOKEN"])
	}
}

func TestVersionWithoutBinary(t *testing.T) {
	r, _ := newTestRunner(t)
	if got := r.Version(false); got != "" {
		t.Fatalf("Version without binary = %q, want \"\"", got)
	}
	if got := r.Version(true); got != "" {
		t.Fatalf("Version(refresh) without binary = %q, want \"\"", got)
	}
}

func TestVersionParsesOutput(t *testing.T) {
	r, p := newTestRunner(t)
	fsx.MkdirAll(filepath.Dir(p.CFBin), 0o755)
	if err := os.WriteFile(p.CFBin, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.execFn = func(args []string, _ ExecOptions) ExecResult {
		calls++
		return ExecResult{Stdout: "cloudflared version 2024.10.0 (built 2024-10-01)\n"}
	}

	if got := r.Version(false); got != "2024.10.0" {
		t.Fatalf("Version = %q, want 2024.10.0", got)
	}
	if got := r.Version(false); got != "2024.10.0" {
		t.Fatalf("cached Version = %q", got)
	}
	if calls != 1 {
		t.Fatalf("exec calls = %d, want 1 (cached)", calls)
	}
	if got := r.Version(true); got != "2024.10.0" {
		t.Fatalf("refreshed Version = %q", got)
	}
	if calls != 2 {
		t.Fatalf("exec calls = %d, want 2 (refresh re-runs)", calls)
	}
}

func TestAppVersion(t *testing.T) {
	r, p := newTestRunner(t)
	if got := r.AppVersion(); got != "" {
		t.Fatalf("missing manifest = %q, want \"\"", got)
	}

	fsx.MkdirAll(p.AppDest, 0o755)
	manifest := filepath.Join(p.AppDest, "..", "manifest")
	body := "appname               = cloudflared\nversion               = 1.4.2\ninstall_dep_apps      = nodejs_v24\n"
	if err := os.WriteFile(manifest, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.AppVersion(); got != "1.4.2" {
		t.Fatalf("AppVersion = %q, want 1.4.2", got)
	}
}

func TestExecMissingBinary(t *testing.T) {
	r, p := newTestRunner(t)
	fsx.MkdirAll(p.PkgVar, 0o755)

	res := r.Exec([]string{"--version"}, ExecOptions{})
	if res.Code == 0 {
		t.Fatalf("expected a failure, got %#v", res)
	}
	if res.Err == "" {
		t.Fatalf("expected an error message, got %#v", res)
	}
}

func TestUpdateArch(t *testing.T) {
	arch, err := updateArch()
	if err != nil {
		t.Skipf("unsupported arch on this host: %v", err)
	}
	if arch != "amd64" && arch != "arm64" {
		t.Fatalf("arch = %q, want amd64 or arm64", arch)
	}
}
