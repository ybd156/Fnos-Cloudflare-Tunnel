// Follow-the-latest support for the pinned cloudflared binary.
//
// The pin in tools/internal/cfpin is what every build verifies against, and
// tools/fpk re-checks the packaged bytes against it. That is only useful if the
// pin can be moved forward safely, so this file adds the one operation that
// does it: ask GitHub for the newest release, download that asset, hash it,
// prove it is a Linux ELF, and only then rewrite the pin.
//
// It exists because a hard-coded pin goes stale: the package shipped 2026.9.3
// while upstream had already published 2026.10.7, and bumping it by hand means
// downloading 40MB, hashing it, and editing a Go literal without a typo.
package fetchcf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ybd156/fnos-cloudflared/tools/internal/cfpin"
)

const latestAPI = "https://api.github.com/repos/cloudflare/cloudflared/releases/latest"

// LatestTag asks GitHub which cloudflared release is newest. GITHUB_TOKEN is
// used when present: the unauthenticated limit is 60 requests per hour, which a
// scheduled workflow would eventually hit.
func LatestTag() (string, error) {
	req, err := http.NewRequest(http.MethodGet, latestAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "fnos-cloudflared-build")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach the GitHub API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("cannot parse the GitHub response: %w", err)
	}
	if payload.TagName == "" {
		return "", errors.New("the GitHub response carries no tag_name")
	}
	return payload.TagName, nil
}

// FollowLatest moves the pin to the newest upstream release and fetches it.
//
// Nothing is written until the download has been hashed and proven to be a
// Linux ELF: a truncated body, an HTML error page, or an asset renamed upstream
// must never become the pin.
func FollowLatest(root string, stdout, stderr io.Writer) error {
	tag, err := LatestTag()
	if err != nil {
		return err
	}
	current := cfpin.Pinned
	if tag == current.Version {
		fmt.Fprintf(stdout, "  cloudflared %s is already the newest release\n", tag)
		return Run(root, Options{}, stdout, stderr)
	}

	next := cfpin.Pin{Version: tag, Asset: current.Asset}
	dest := cfpin.Dest(root)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", filepath.Dir(dest), err)
	}

	part := dest + ".part"
	defer os.Remove(part) // no-op once the rename succeeded

	fmt.Fprintf(stdout, "  newer release %s (pinned: %s)\n", tag, current.Version)
	fmt.Fprintf(stdout, "  downloading %s\n", next.URL())
	if _, err := download(next.URL(), part, stdout); err != nil {
		return err
	}
	if err := verifyELF(part); err != nil {
		return err
	}
	sum, err := cfpin.HashFile(part)
	if err != nil {
		return fmt.Errorf("cannot hash the download: %w", err)
	}
	next.SHA256 = sum

	if err := writePin(root, next); err != nil {
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("cannot move the download into place: %w", err)
	}
	if err := os.Chmod(dest, destMode); err != nil {
		return fmt.Errorf("cannot make %s executable: %w", dest, err)
	}
	fmt.Fprintf(stdout, "  pinned %s, sha256 %s\n", next.Version, sum)
	return nil
}

// verifyELF rejects anything that is not a Linux ELF executable. The release
// asset is a static binary; an HTML error page saved under the same name would
// otherwise be hashed, pinned, and shipped.
func verifyELF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("cannot read the download: %w", err)
	}
	if string(magic[:]) != "\x7fELF" {
		return fmt.Errorf("the download is not a Linux ELF (first bytes %q)", magic)
	}
	return nil
}

var (
	pinVersionRE = regexp.MustCompile(`(?m)^(\tVersion:\s*)"[^"]*"`)
	pinSHA256RE  = regexp.MustCompile(`(?m)^(\tSHA256:\s*)"[^"]*"`)
)

// writePin rewrites the two literals in cfpin.go. The struct fields carry the
// same names, so both patterns require the opening quote of a value.
func writePin(root string, pin cfpin.Pin) error {
	path := filepath.Join(root, "tools", "internal", "cfpin", "cfpin.go")
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read the pin source: %w", err)
	}
	text := string(src)
	updated := pinVersionRE.ReplaceAllString(text, `${1}"`+pin.Version+`"`)
	updated = pinSHA256RE.ReplaceAllString(updated, `${1}"`+pin.SHA256+`"`)
	if updated == text {
		return errors.New("cfpin.go carries no Version/SHA256 literal to update")
	}
	if !strings.Contains(updated, `"`+pin.Version+`"`) || !strings.Contains(updated, `"`+pin.SHA256+`"`) {
		return errors.New("the pin rewrite did not take effect")
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}
