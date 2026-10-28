package cf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/config"
	"github.com/ybd156/fnos-cloudflared/internal/fsx"
)

// isoLayout is the timestamp format the previous implementation emitted for
// startedAt (Date#toISOString).
const isoLayout = "2006-01-02T15:04:05.000Z"

func nowISO() string { return time.Now().UTC().Format(isoLayout) }

// LoginStatus is the state of the interactive login workflow.
type LoginStatus struct {
	LoggedIn  bool   `json:"loggedIn"`
	Status    string `json:"status"`
	URL       string `json:"url"`
	Error     string `json:"error"`
	StartedAt string `json:"startedAt"`
	CertPath  string `json:"certPath"`
}

// loginState is the mutable half of LoginStatus plus the running child.
//
// gen increments on every (re)start and on logout. The goroutines watching a
// child capture the generation they belong to, so a late exit notification from
// a logged-out session cannot overwrite the new state with a stale error.
type loginState struct {
	gen       int
	status    string
	url       string
	errMsg    string
	startedAt string
	cmd       *exec.Cmd
	stop      chan struct{}
}

// AccountTunnel is one tunnel as listed by the Cloudflare account.
type AccountTunnel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CreatedAt   string `json:"createdAt"`
	Connections int    `json:"connections"`
}

var (
	// The login page URL cloudflared prints for the user to open.
	loginURLRE = regexp.MustCompile(`https://[^\s"'<>]+`)
	// A URL that ends a sentence keeps its full stop; cloudflared never
	// generates a URL that legitimately ends in one of these.
	urlTrailerRE = regexp.MustCompile(`[.,)]+$`)
)

// LoginStart begins `cloudflared tunnel login` and returns as soon as the
// process is running; the URL and the outcome are reported through
// LoginStatus.
//
// A failed spawn is reported the same way the previous implementation did: the
// status becomes "error" with the message, and this function returns nil. The
// returned error is reserved for a failure to prepare the state directory.
func (r *Runner) LoginStart() error {
	// The whole preparation is serialised: two concurrent starts would
	// otherwise leave two login children and one orphaned poller.
	r.loginMu.Lock()
	defer r.loginMu.Unlock()

	if r.login.cmd != nil {
		// Already running: the caller polls LoginStatus for progress.
		return nil
	}
	if err := os.MkdirAll(r.p.CFDir, 0o700); err != nil {
		return err
	}
	// A stale cert.pem would make the next status poll report success before
	// the user has done anything.
	fsx.UnlinkQuiet(r.p.CertFile)

	cmd := exec.Command(r.p.CFBin, "tunnel", "login")
	cmd.Dir = r.p.PkgVar
	cmd.Env = r.Env(nil)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.login.status = "error"
		r.login.errMsg = err.Error()
		return nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		r.login.status = "error"
		r.login.errMsg = err.Error()
		return nil
	}

	stop := make(chan struct{})
	r.login.gen++
	gen := r.login.gen
	r.login.status = "waiting"
	r.login.url = ""
	r.login.errMsg = ""
	r.login.startedAt = nowISO()
	r.login.stop = stop

	if err := cmd.Start(); err != nil {
		r.login.status = "error"
		r.login.errMsg = err.Error()
		r.login.cmd = nil
		return nil
	}
	r.login.cmd = cmd

	go r.watchLogin(gen, cmd, stdout, stderr)
	go r.pollCert(gen, stop)
	return nil
}

// watchLogin reads both pipes (so the child can never block on a full pipe),
// captures the login URL and records how the process ended.
func (r *Runner) watchLogin(gen int, cmd *exec.Cmd, stdout, stderr io.Reader) {
	var wg sync.WaitGroup
	// The URL can straddle a read boundary, so each stream is matched against
	// everything seen so far rather than against one chunk.
	var mu sync.Mutex
	seen := ""
	scan := func(rd io.Reader) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				mu.Lock()
				if len(seen) < 64<<10 {
					seen += string(buf[:n])
					r.captureLoginURL(gen, seen)
				}
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)
	wg.Wait()

	code := 0
	if err := cmd.Wait(); err != nil {
		code = 1
		if c := exitCode(err); c > 0 {
			code = c
		}
	}
	r.finishLogin(gen, code)
}

func (r *Runner) captureLoginURL(gen int, text string) {
	m := loginURLRE.FindString(text)
	if m == "" {
		return
	}
	m = urlTrailerRE.ReplaceAllString(m, "")

	r.loginMu.Lock()
	first := false
	if r.login.gen == gen && r.login.url == "" {
		r.login.url = m
		first = true
	}
	r.loginMu.Unlock()
	if first {
		r.info("cloudflared login URL captured")
	}
}

func (r *Runner) finishLogin(gen, code int) {
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	if r.login.gen != gen {
		return
	}
	r.login.cmd = nil

	if fsx.Exists(r.p.CertFile) {
		r.login.status = "done"
		r.login.errMsg = ""
		return
	}
	if r.login.status == "error" {
		return
	}
	// Exit code 0 means the user closed the browser page without finishing the
	// authorisation -- a cancellation, not a failure. Only a non-zero exit is an
	// actual error.
	r.login.status = "error"
	if code == 0 {
		r.login.errMsg = "登录已取消，未获取到 cert.pem"
	} else {
		r.login.errMsg = fmt.Sprintf("登录进程退出（退出码 %d），未获取到 cert.pem", code)
	}
}

// pollCert treats the appearance of cert.pem as the authoritative success
// signal, independent of what the process prints.
func (r *Runner) pollCert(gen int, stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		if fsx.Exists(r.p.CertFile) {
			r.loginMu.Lock()
			var cmd *exec.Cmd
			if r.login.gen == gen {
				r.login.status = "done"
				r.login.errMsg = ""
				cmd = r.login.cmd
			}
			r.loginMu.Unlock()
			if cmd != nil && cmd.Process != nil {
				// cloudflared exits by itself once the certificate is written;
				// this only covers a build that lingers.
				_ = cmd.Process.Signal(os.Interrupt)
			}
			return
		}

		r.loginMu.Lock()
		done := r.login.gen != gen || (r.login.cmd == nil && r.login.status != "done")
		r.loginMu.Unlock()
		if done {
			return
		}
	}
}

// LoginStatus reports the current state. cert.pem on disk always wins: it is
// the only fact that survives a restart of the service.
func (r *Runner) LoginStatus() LoginStatus {
	loggedIn := fsx.Exists(r.p.CertFile)

	r.loginMu.Lock()
	state := r.login
	r.loginMu.Unlock()

	status := state.status
	if loggedIn {
		status = "done"
	}
	return LoginStatus{
		LoggedIn:  loggedIn,
		Status:    status,
		URL:       state.url,
		Error:     state.errMsg,
		StartedAt: state.startedAt,
		CertPath:  r.p.CertFile,
	}
}

// Logout stops any running login, forgets the state and deletes cert.pem.
func (r *Runner) Logout() {
	r.loginMu.Lock()
	cmd := r.login.cmd
	stop := r.login.stop
	r.login.gen++
	r.login = loginState{gen: r.login.gen, status: "idle"}
	r.loginMu.Unlock()

	if stop != nil {
		close(stop)
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	fsx.UnlinkQuiet(r.p.CertFile)
}

// ListTunnels returns the tunnels of the logged-in account.
func (r *Runner) ListTunnels() ([]AccountTunnel, error) {
	if !fsx.Exists(r.p.CertFile) {
		return nil, errors.New("尚未登录 Cloudflare 账户")
	}
	// `--output` is a *subcommand* flag in cloudflared's CLI, so it has to
	// follow `list`; before the subcommand it is rejected as an unknown tunnel
	// option.
	res := r.Exec([]string{"tunnel", "list", "--output", "json"},
		ExecOptions{Timeout: 30 * time.Second})
	if res.Code != 0 {
		msg := tailLines(firstNonEmpty(res.Stderr, res.Stdout, res.Err), 3)
		if msg == "" {
			msg = fmt.Sprintf("cloudflared 退出码 %d", res.Code)
		}
		return nil, errors.New(msg)
	}

	var raw []accountTunnelJSON
	if err := json.Unmarshal(parseJSONBody(res.Stdout), &raw); err != nil {
		// Some versions wrap the list in an object.
		var wrapper struct {
			Tunnels []accountTunnelJSON `json:"tunnels"`
		}
		if err2 := json.Unmarshal(parseJSONBody(res.Stdout), &wrapper); err2 != nil {
			raw = nil
		} else {
			raw = wrapper.Tunnels
		}
	}

	out := make([]AccountTunnel, 0, len(raw))
	for _, t := range raw {
		createdAt := t.CreatedAt
		if createdAt == "" {
			createdAt = t.CreatedAtAlt
		}
		out = append(out, AccountTunnel{
			ID:          t.ID,
			Name:        t.Name,
			CreatedAt:   createdAt,
			Connections: len(t.Connections),
		})
	}
	return out, nil
}

type accountTunnelJSON struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	CreatedAt    string            `json:"createdAt"`
	CreatedAtAlt string            `json:"created_at"`
	Connections  []json.RawMessage `json:"connections"`
}

// CreateTunnel creates a tunnel on the account and returns cloudflared's own
// JSON object (nil when it printed something unparseable) plus its output.
func (r *Runner) CreateTunnel(name string) (raw json.RawMessage, output string, err error) {
	if !fsx.Exists(r.p.CertFile) {
		return nil, "", errors.New("尚未登录 Cloudflare 账户")
	}
	if !config.ValidTunnelName(name) {
		return nil, "", errors.New("隧道名称只能包含字母、数字、点、下划线和连字符")
	}

	res := r.Exec([]string{"tunnel", "create", "--output", "json", name},
		ExecOptions{Timeout: 60 * time.Second})
	combined := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	if res.Code != 0 {
		msg := tailLines(combined, 3)
		if msg == "" {
			msg = fmt.Sprintf("退出码 %d", res.Code)
		}
		return nil, combined, errors.New(msg)
	}
	return parseJSONRaw(res.Stdout), combined, nil
}

// DeleteTunnel deletes a tunnel from the account.
func (r *Runner) DeleteTunnel(name string) error {
	if !fsx.Exists(r.p.CertFile) {
		return errors.New("尚未登录 Cloudflare 账户")
	}
	if !config.ValidTunnelName(name) {
		return errors.New("非法的隧道名称")
	}

	res := r.Exec([]string{"tunnel", "delete", "-f", name},
		ExecOptions{Timeout: 60 * time.Second})
	if res.Code != 0 {
		msg := tailLines(firstNonEmpty(res.Stderr, res.Stdout), 3)
		if msg == "" {
			msg = fmt.Sprintf("cloudflared 退出码 %d", res.Code)
		}
		return errors.New(msg)
	}
	return nil
}

// RouteDNS points hostname at the tunnel (--overwrite-dns so a stale A record
// is replaced rather than rejected).
func (r *Runner) RouteDNS(name, hostname string) error {
	if !fsx.Exists(r.p.CertFile) {
		return errors.New("尚未登录 Cloudflare 账户")
	}
	if !config.ValidTunnelName(name) {
		return errors.New("非法的隧道名称")
	}
	if !config.DNSHostnameRE.MatchString(hostname) {
		return errors.New("非法的域名")
	}

	res := r.Exec([]string{"tunnel", "route", "dns", "--overwrite-dns", name, hostname},
		ExecOptions{Timeout: 60 * time.Second})
	if res.Code != 0 {
		msg := tailLines(firstNonEmpty(res.Stderr, res.Stdout), 3)
		if msg == "" {
			msg = fmt.Sprintf("cloudflared 退出码 %d", res.Code)
		}
		return errors.New(msg)
	}
	return nil
}

// firstNonEmpty returns the first non-empty value, the way the previous
// implementation's `stderr || stdout || error` chain did.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// tailLines keeps the last n lines and joins them with spaces: cloudflared
// puts the actual reason in the last lines, and a whole stack trace in a toast
// is unreadable.
func tailLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " ")
}

// jsonStart finds where the JSON payload begins. cloudflared may print log
// lines before it, so the first '[' or '{' is where the value starts.
func jsonStart(text string) int {
	startArr := strings.IndexByte(text, '[')
	startObj := strings.IndexByte(text, '{')
	if startArr >= 0 && (startObj < 0 || startArr < startObj) {
		return startArr
	}
	return startObj
}

// parseJSONBody returns the JSON payload of text, or nil when there is none.
func parseJSONBody(text string) []byte {
	idx := jsonStart(text)
	if idx < 0 {
		return nil
	}
	return []byte(text[idx:])
}

// parseJSONRaw validates the payload and returns it unchanged, so the account
// view can forward cloudflared's own object without re-encoding it.
func parseJSONRaw(text string) json.RawMessage {
	body := bytes.TrimRight(parseJSONBody(text), " \t\r\n")
	if len(body) == 0 || !json.Valid(body) {
		return nil
	}
	return json.RawMessage(body)
}
