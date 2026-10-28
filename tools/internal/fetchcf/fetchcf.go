// Package fetchcf downloads the pinned upstream cloudflared binary and proves
// it is the pinned release before putting it where the build will pick it up.
//
// Port of the old tools/fetch-cloudflared.py. The behaviour that matters is
// preserved exactly: nothing replaces the real file until the SHA-256 of the
// downloaded bytes has been checked, and a correct local copy is reused instead
// of re-downloading 38MB on every build.
package fetchcf

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ybd156/fnos-cloudflared/tools/internal/cfpin"
)

const (
	destMode      = 0o755
	chunk         = 1 << 20
	progressEvery = 8 << 20
	headerTimeout = 120 * time.Second
)

// Options mirrors the old command line.
type Options struct {
	Check bool // verify only; never download
	Force bool // re-fetch even when a correct copy is present
}

// Run fetches or verifies the pinned binary for the checkout at root.
func Run(root string, opt Options, stdout, stderr io.Writer) error {
	return RunWithPin(root, cfpin.Pinned, opt, stdout, stderr)
}

// RunWithPin is Run against an explicit pin, so tests can drive every branch
// (including the mismatch path) without touching the network or the real pin.
func RunWithPin(root string, pin cfpin.Pin, opt Options, stdout, stderr io.Writer) error {
	dest := cfpin.Dest(root)
	rel := relTo(root, dest)

	fmt.Fprintf(stdout, "pinned cloudflared %s (%s...)\n", pin.Version, short(pin.SHA256))

	st, statErr := os.Stat(dest)
	present := statErr == nil && !st.IsDir()

	switch {
	case present && !opt.Force:
		got, err := cfpin.HashFile(dest)
		if err != nil {
			return fmt.Errorf("cannot hash %s: %w", rel, err)
		}
		if got == pin.SHA256 {
			fmt.Fprintf(stdout, "  %s already present and matches the pin\n", rel)
			return nil
		}
		fmt.Fprintf(stdout, "  present but the hash differs:\n    have %s\n    want %s\n", got, pin.SHA256)
		if opt.Check {
			fmt.Fprintln(stderr, "  --check: not downloading")
			return errors.New("the present binary does not match the pin")
		}
		fmt.Fprintln(stdout, "  re-fetching")
	case opt.Check:
		fmt.Fprintf(stderr, "  %s is missing\n", rel)
		return errors.New("the pinned binary is missing")
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(rel), err)
	}

	part := dest + ".part"
	defer os.Remove(part) // no-op once the rename succeeded

	fmt.Fprintf(stdout, "  downloading %s\n", pin.URL())
	size, err := download(pin.URL(), part, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "  download failed: %v\n", err)
		fmt.Fprintf(stderr, "  fetch it by hand and place it at %s:\n", rel)
		fmt.Fprintf(stderr, "    curl -L -o %s %s\n", rel, pin.URL())
		fmt.Fprintf(stderr, "    expected sha256: %s\n", pin.SHA256)
		return err
	}

	got, err := cfpin.HashFile(part)
	if err != nil {
		return fmt.Errorf("cannot hash the download: %w", err)
	}
	if got != pin.SHA256 {
		fmt.Fprintf(stderr, "  SHA-256 mismatch:\n    got  %s\n    want %s\n", got, pin.SHA256)
		return errors.New("the downloaded binary does not match the pin")
	}

	// Only replace the real file once the bytes are proven: a truncated or
	// tampered download must never land where the build will pick it up.
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("cannot move the download into place: %w", err)
	}
	_ = os.Chmod(dest, destMode)
	fmt.Fprintf(stdout, "  ok: %d bytes, sha256 verified\n", size)
	return nil
}

// download streams url into part, reporting progress every 8MB.
func download(url, part string, out io.Writer) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "fnos-cloudflared-build")

	// A whole-request Timeout would abort a slow 38MB transfer on a home
	// connection; the old script used a per-read timeout, so bound the phases
	// that can hang instead.
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
	}}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s: %s", url, resp.Status)
	}

	f, err := os.Create(part)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	total := resp.ContentLength
	var done int64
	nextMark := int64(progressEvery)
	buf := make([]byte, chunk)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return done, err
			}
			done += int64(n)
			if done >= nextMark {
				nextMark += progressEvery
				pct := ""
				if total > 0 {
					pct = fmt.Sprintf(" (%.0f%%)", float64(done)*100/float64(total))
				}
				fmt.Fprintf(out, "  ... %.0f MB%s\n", float64(done)/(1<<20), pct)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return done, readErr
		}
	}
	if err := f.Close(); err != nil {
		return done, err
	}
	return done, nil
}

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return strings.TrimSpace(sha[:12])
}
