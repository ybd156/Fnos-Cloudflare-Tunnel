package cf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/fsx"
)

const (
	releaseAPI     = "https://api.github.com/repos/cloudflare/cloudflared/releases/latest"
	userAgent      = "fnos-cloudflared"
	apiTimeout     = 60 * time.Second
	downloadTimout = 10 * time.Minute
)

// appVersionRE matches the `version = 1.4.2` line of the fnpack manifest.
var appVersionRE = regexp.MustCompile(`(?m)^version\s*=\s*(.+)$`)

// AppVersion reports the version of the installed package, read from the
// manifest one level above the payload. It returns "" when the file is not
// there, which is the normal case when running the binary by hand.
func (r *Runner) AppVersion() string {
	data, err := os.ReadFile(filepath.Join(r.p.AppDest, "..", "manifest"))
	if err != nil {
		return ""
	}
	m := appVersionRE.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// releaseAsset is the one field of a release asset this needs.
type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type releaseInfo struct {
	Assets []releaseAsset `json:"assets"`
}

// UpdateBinary replaces the cloudflared binary with the newest upstream release.
//
// The new binary is written to $PKGVAR, not to the payload. The payload
// directory is replaced wholesale on every install and upgrade, so a binary
// updated inside app/bin is silently reverted by the next package install: a
// user who updated to a newer release found the bundled one back after upgrading
// the app. $PKGVAR survives upgrades, and paths.ActiveCFBin prefers it.
func (r *Runner) UpdateBinary() (string, error) {
	arch, err := updateArch()
	if err != nil {
		return "", err
	}

	release, err := fetchRelease()
	if err != nil {
		return "", err
	}
	want := "cloudflared-linux-" + arch
	var asset *releaseAsset
	for i := range release.Assets {
		if release.Assets[i].Name == want {
			asset = &release.Assets[i]
			break
		}
	}
	if asset == nil {
		return "", fmt.Errorf("未找到适配 %s 的发布文件", arch)
	}

	fsx.MkdirAll(filepath.Dir(r.p.CFBinUpdated), 0o755)
	tmp := filepath.Join(r.p.PkgTmp, fmt.Sprintf("cloudflared-%d", time.Now().UnixMilli()))
	defer os.Remove(tmp)

	if err := download(asset.URL, tmp); err != nil {
		return "", err
	}
	fsx.ChmodQuiet(tmp, 0o755)

	// Prove the download actually runs before it replaces a working binary. A
	// truncated file or the wrong architecture would otherwise leave the app
	// with a cloudflared that cannot start a single tunnel, and the failure
	// would only surface on the next start.
	if err := verifyBinary(tmp); err != nil {
		return "", err
	}

	data, err := os.ReadFile(tmp)
	if err != nil {
		return "", fmt.Errorf("读取下载文件失败: %s", err.Error())
	}
	// Atomic replace: a tunnel restarting at this instant must never see a
	// half-written binary.
	if err := fsx.AtomicWrite(r.p.CFBinUpdated, data, 0o755); err != nil {
		return "", fmt.Errorf("替换 cloudflared 失败: %s", err.Error())
	}

	version := r.Version(true)
	if err := fsx.AtomicWrite(r.p.CFVersionFile, []byte(version+"\n"), 0o644); err != nil {
		return version, fmt.Errorf("记录 cloudflared 版本失败: %s", err.Error())
	}
	r.info("cloudflared binary updated to", version, "("+r.p.CFBinUpdated+")")
	return version, nil
}

// ResetBinary drops the updated binary so the bundled one is used again.
//
// Without this a bad update would be permanent: the only other way back is
// reinstalling the whole package.
func (r *Runner) ResetBinary() (string, error) {
	if fsx.Exists(r.p.CFBinUpdated) {
		if err := os.Remove(r.p.CFBinUpdated); err != nil {
			return "", fmt.Errorf("删除已更新的 cloudflared 失败: %s", err.Error())
		}
	}
	version := r.Version(true)
	if err := fsx.AtomicWrite(r.p.CFVersionFile, []byte(version+"\n"), 0o644); err != nil {
		return version, fmt.Errorf("记录 cloudflared 版本失败: %s", err.Error())
	}
	r.info("reverted to the bundled cloudflared", version)
	return version, nil
}

// verifyBinary runs `--version` on a candidate binary.
func verifyBinary(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return fmt.Errorf("下载的 cloudflared 无法执行: %s", text)
	}
	if !strings.Contains(text, "cloudflared version") {
		return fmt.Errorf("下载的 cloudflared 未报告版本信息: %s", text)
	}
	return nil
}

// updateArch maps the Go architecture onto the asset name cloudflared
// publishes. The previous implementation mapped Node's x64/arm64 names.
func updateArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("不支持的架构: %s", runtime.GOARCH)
	}
}

// fetchRelease reads the latest release metadata. Redirects are followed by
// net/http itself, which replaces the hand-written recursion of the previous
// implementation.
func fetchRelease() (releaseInfo, error) {
	var out releaseInfo

	client := &http.Client{Timeout: apiTimeout}
	req, err := http.NewRequest(http.MethodGet, releaseAPI, nil)
	if err != nil {
		return out, fmt.Errorf("请求 GitHub 失败: %s", err.Error())
	}
	req.Header.Set("User-Agent", userAgent)

	res, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("请求 GitHub 失败: %s", err.Error())
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("请求 GitHub 失败 (HTTP %d)", res.StatusCode)
	}
	// A body larger than any plausible release listing is a sign of something
	// other than the API answering; refuse rather than buffer it.
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return out, fmt.Errorf("读取 GitHub 响应失败: %s", err.Error())
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, errors.New("无法解析 GitHub 响应")
	}
	return out, nil
}

// download streams url into dest.
func download(url, dest string) error {
	client := &http.Client{Timeout: downloadTimout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("下载 cloudflared 失败: %s", err.Error())
	}
	req.Header.Set("User-Agent", userAgent)

	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载 cloudflared 失败: %s", err.Error())
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 cloudflared 失败 (HTTP %d)", res.StatusCode)
	}

	file, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("下载 cloudflared 失败: %s", err.Error())
	}
	// 128 MB is far above the ~40 MB binary and still bounds a hostile or
	// broken response.
	if _, err := io.Copy(file, io.LimitReader(res.Body, 128<<20)); err != nil {
		file.Close()
		os.Remove(dest)
		return fmt.Errorf("下载 cloudflared 失败: %s", err.Error())
	}
	if err := file.Close(); err != nil {
		os.Remove(dest)
		return fmt.Errorf("下载 cloudflared 失败: %s", err.Error())
	}
	return nil
}
