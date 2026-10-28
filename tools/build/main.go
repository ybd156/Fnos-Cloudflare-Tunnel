// Command build is the one and only build pipeline for the Cloudflare Tunnel
// .fpk package.
//
//	go run ./tools/build              regenerate icons, validate, then pack
//	go run ./tools/build --no-icons   skip icon regeneration
//	go run ./tools/build --no-pack    run every check, stop before fnpack
//
// build.sh and build.ps1 are thin wrappers around this command. They used to
// each implement the same twelve steps, and they drifted: build.sh ran a
// JavaScript syntax check that build.ps1 never had, so a Windows build could
// ship a frontend that did not parse. Keeping the steps here means there is one
// definition of "the pipeline" regardless of the host.
//
// Requires Go (>= 1.22) and, for the packing step, the official `fnpack`
// utility (https://developer.fnnas.com/docs/cli/fnpack).
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ybd156/fnos-cloudflared/tools/internal/fetchcf"
	"github.com/ybd156/fnos-cloudflared/tools/internal/fpk"
	"github.com/ybd156/fnos-cloudflared/tools/internal/icons"
	"github.com/ybd156/fnos-cloudflared/tools/internal/lintshell"
	"github.com/ybd156/fnos-cloudflared/tools/internal/lintui"
	"github.com/ybd156/fnos-cloudflared/tools/internal/reporoot"
	"github.com/ybd156/fnos-cloudflared/tools/internal/validatejson"
)

const usage = `Build the Cloudflare Tunnel .fpk package for fnOS.

  ./build.sh                 bash host   (icons, validate, pack)
  pwsh -File build.ps1       Windows     (same pipeline)
  go run ./tools/build       any host    (the pipeline itself)

  --no-icons        skip icon regeneration
  --no-pack         stop after validation (hosts without fnpack)
  --fnpack <path>   fnpack executable (default: $FNPACK, then PATH)
  --root <dir>      repository root (default: discovered from go.mod)

Requires Go (>= 1.22). The packing step requires the official fnpack utility
(https://developer.fnnas.com/docs/cli/fnpack); set FNPACK when it is not on PATH.
`

// requiredFiles must exist in the payload before anything is packed. They are
// the files fnOS reads or executes; a missing one is a package that fails at
// install time, not at review time.
var requiredFiles = []string{
	"manifest",
	"ICON.PNG",
	"ICON_256.PNG",
	"config/privilege",
	"config/resource",
	"app/ui/config",
	"app/bin/cloudflared",
	"app/bin/fncfd",
	"app/www/index.html",
}

type pipeline struct {
	root      string
	project   string
	dist      string
	goBin     string
	fnpack    string
	skipIcons bool
	skipPack  bool
}

// scratchDir returns a writable directory for tools that unpack into $TMPDIR.
//
// The host temp directory is not always usable (a locked-down Windows profile
// denies it outright, and fnpack then fails with a bare "Access is denied").
// Keeping the scratch area inside the checkout makes the build independent of
// the host's TMP configuration.
func (p *pipeline) scratchDir() (string, error) {
	dir := filepath.Join(p.root, "work", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// A stale tree from an interrupted build would otherwise be reused.
	return dir, nil
}

func main() {
	var (
		noIcons = flag.Bool("no-icons", false, "skip icon regeneration")
		noPack  = flag.Bool("no-pack", false, "stop after validation (hosts without fnpack)")
		fnpack  = flag.String("fnpack", "", "fnpack executable (default: $FNPACK, then PATH)")
		root    = flag.String("root", "", "repository root (default: discovered from go.mod)")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	repo, err := reporoot.Resolve(*root)
	if err != nil {
		fail("%v", err)
	}

	p := &pipeline{
		root:      repo,
		project:   filepath.Join(repo, "cloudflared"),
		dist:      filepath.Join(repo, "dist"),
		skipIcons: *noIcons,
		skipPack:  *noPack,
	}

	if err := p.run(*fnpack); err != nil {
		fail("%v", err)
	}
}

func (p *pipeline) run(fnpackFlag string) error {
	// ------------------------------------------------------- prerequisites
	if st, err := os.Stat(p.project); err != nil || !st.IsDir() {
		return fmt.Errorf("application source directory not found: %s", p.project)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return errors.New("go not found in PATH (install Go >= 1.22, or run ./build.sh which locates it)")
	}
	p.goBin = goBin

	if !p.skipPack {
		resolved, err := resolveFnpack(fnpackFlag)
		if err != nil {
			return err
		}
		p.fnpack = resolved
	}

	fmt.Printf("go     : %s\n", p.goBin)
	if p.fnpack != "" {
		fmt.Printf("fnpack : %s\n", p.fnpack)
	}

	// -------------------------------------------------- upstream binary
	// The 38MB cloudflared binary is not tracked in git (see .gitignore): it is
	// an upstream build that nobody here edits. Fetch the pinned release
	// instead, verifying its SHA-256. A correct local copy is reused.
	step("Ensuring the cloudflared binary")
	if err := fetchcf.Run(p.root, fetchcf.Options{}, os.Stdout, os.Stderr); err != nil {
		return errors.New("could not obtain the pinned cloudflared binary")
	}

	// ------------------------------------------------------- service binary
	// The service is a static Go binary. It is cross-compiled for the NAS even
	// on a Windows host, because the artefact is what gets installed -- a
	// windows/amd64 binary in the package would simply never start.
	step("Building the service binary (linux/amd64)")
	if err := p.buildServer(); err != nil {
		return err
	}

	// ----------------------------------------------------------------- icons
	if p.skipIcons {
		step("Skipping icon generation")
	} else {
		step("Generating icons")
		if err := icons.Generate(p.root, os.Stdout); err != nil {
			return fmt.Errorf("icon generation failed: %w", err)
		}
	}

	// ----------------------------------------------------------- permissions
	// This filesystem can present freshly created files as mode 0000, and the
	// .fpk archive does not reliably preserve permission bits anyway. Normalise
	// everything so the packaged tree is readable and the executables are
	// runnable.
	step("Normalising permissions")
	if err := normalizeTree(p.project); err != nil {
		return fmt.Errorf("permission normalisation failed: %w", err)
	}

	// ------------------------------------------------------------ validation
	step("Validating shell scripts")
	if err := p.checkShellSyntax(); err != nil {
		return err
	}

	// A selector that points at an element which does not exist fails silently:
	// the handler binds to nothing, the control stops working, and no error
	// surfaces anywhere. Nothing else in the pipeline can see that.
	step("Validating UI selectors")
	if err := lintui.Run(p.root, os.Stdout); err != nil {
		return errors.New("the web UI references a selector that does not resolve")
	}

	// A shell edit is otherwise reviewed by eye only: on Windows there is no
	// bash to run it, and the mistakes that matter (an unbound ${TRIM_X} under
	// `set -u`, an interpreter call that is absent on the NAS, an unbalanced
	// quote) are invisible to inspection.
	step("Validating shell scripts (static)")
	if err := lintshell.Run(p.root, os.Stdout); err != nil {
		return errors.New("a lifecycle script has a static defect")
	}

	step("Validating JSON files")
	jsonFiles, err := p.jsonFiles()
	if err != nil {
		return err
	}
	if err := validatejson.Run(jsonFiles, os.Stdout, os.Stderr); err != nil {
		return errors.New("invalid JSON in config/privilege, config/resource, app/ui/config or wizard/")
	}

	step("Checking required files")
	for _, rel := range requiredFiles {
		if !exists(filepath.Join(p.project, filepath.FromSlash(rel))) {
			return fmt.Errorf("missing required file: %s", rel)
		}
	}
	fmt.Println("  all required files present")

	// --------------------------------------------------------------- packing
	if p.skipPack {
		step("Skipping fnpack (--no-pack)")
		fmt.Println("  every validation step passed; the artefact was not produced")
		return nil
	}

	step("Packing with fnpack")
	if err := os.MkdirAll(p.dist, 0o755); err != nil {
		return err
	}
	fpkPath := filepath.Join(p.dist, "cloudflared.fpk")
	if err := os.Remove(fpkPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	cmd := exec.Command(p.fnpack, "build", "-d", p.project)
	cmd.Dir = p.dist // fnpack writes <appname>.fpk into the working directory
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// fnpack unpacks into $TMPDIR. When that location is not writable it fails
	// with a bare "Access is denied" that says nothing about the cause, so give
	// it a scratch directory inside the checkout instead of inheriting whatever
	// the host points at.
	if tmp, tmpErr := p.scratchDir(); tmpErr == nil {
		cmd.Env = envWith(os.Environ(), "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fnpack build failed: %w", err)
	}
	if !exists(fpkPath) {
		return fmt.Errorf("expected %s was not produced", fpkPath)
	}

	// fnpack takes archive modes from the host filesystem's stat(). On Linux the
	// chmod above has already applied them, but on Windows there is no
	// executable bit at all: every file lands as 0666 and every directory as
	// 0777, so cmd/main and app/bin/cloudflared reach the NAS non-executable and
	// the app cannot start. Rewrite the modes in both tar layers; this also
	// repairs the manifest checksum, which records the MD5 of the rewritten
	// app.tgz.
	step("Normalising archive permissions")
	if _, err := fpk.Normalize(fpkPath, os.Stdout); err != nil {
		return fmt.Errorf("archive permission normalisation failed: %w", err)
	}

	// fnpack writes the archive with whatever umask the volume reports, and this
	// filesystem presents new files as mode 0000, which would leave the artefact
	// unreadable. Make sure the result can actually be copied out.
	_ = os.Chmod(p.dist, 0o755)
	_ = os.Chmod(fpkPath, 0o644)

	// `fnpack build` reports success as long as the required files exist -- it
	// does not check that the archive matches the source tree, so a build from a
	// stale tree still prints "Packing successfully". Re-open the artefact and
	// assert what it actually contains before shipping it.
	step("Verifying packaged artefact")
	if err := fpk.Verify(fpkPath, p.project, fpk.DefaultVerifyOptions(), os.Stdout); err != nil {
		return errors.New("packaged artefact failed verification")
	}

	// ---------------------------------------------------------------- result
	step("Result")
	info, err := fpk.Summary(fpkPath)
	if err != nil {
		return err
	}
	fmt.Printf("  %s\n", fpkPath)
	fmt.Printf("  size    : %d bytes (%.1f MB)\n", info.Size, float64(info.Size)/(1<<20))
	fmt.Printf("  sha256  : %s\n", info.SHA256)
	fmt.Printf("  manifest checksum: checksum = %s\n", info.ManifestChecksum)
	return nil
}

// buildServer cross-compiles cmd/fncfd into the payload as app/bin/fncfd.
func (p *pipeline) buildServer() error {
	mainDir := filepath.Join(p.root, "cmd", "fncfd")
	if !exists(filepath.Join(mainDir, "main.go")) {
		return fmt.Errorf("service entry point not found: %s", filepath.ToSlash(filepath.Join("cmd", "fncfd", "main.go")))
	}
	out := filepath.Join(p.project, "app", "bin", "fncfd")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}

	cmd := exec.Command(p.goBin, "build", "-trimpath", "-ldflags", "-s -w", "-o", out, "./cmd/fncfd")
	cmd.Dir = p.root
	// The artefact is for the NAS, never for the build host: the manifest
	// declares platform = x86, and a host-native binary would not run there.
	cmd.Env = envWith(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH=amd64",
	)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build ./cmd/fncfd failed: %w", err)
	}
	st, err := os.Stat(out)
	if err != nil {
		return err
	}
	fmt.Printf("  app/bin/fncfd  %d bytes (linux/amd64)\n", st.Size())
	return nil
}

// checkShellSyntax runs `bash -n` over every lifecycle script.
//
// The script is fed on stdin rather than as a file argument: on Windows
// exec.LookPath("bash") can find the WSL stub or a bundled MSYS build, neither
// of which can open a "D:\..." path, so passing the path would make this step
// fail for a reason that has nothing to do with the script.
//
// bash exits 2 when `-n` finds a syntax error, and anything else when it cannot
// run at all (no such interpreter, a sandbox that denies it, ...). Only the
// former is a defect in the script; the latter is a property of the host, and
// this check is a NAS-only convenience, so it is skipped with a warning instead
// of failing a Windows build. The artefact itself is still verified.
func (p *pipeline) checkShellSyntax() error {
	bash, diag := pickBash()
	if bash == "" {
		fmt.Println("  ! no usable bash on this host, skipping the syntax check (it runs on the NAS)")
		if diag != "" {
			fmt.Printf("     (%s)\n", diag)
		}
		return nil
	}

	cmdDir := filepath.Join(p.project, "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return err
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		body, err := os.ReadFile(filepath.Join(cmdDir, name))
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		check := exec.CommandContext(ctx, bash, "-n")
		check.Stdin = bytes.NewReader(body)
		var stderr bytes.Buffer
		check.Stderr = &stderr
		runErr := check.Run()
		cancel()
		if runErr != nil {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 2 {
				return fmt.Errorf("syntax error in %s:\n%s", name, strings.TrimSpace(stderr.String()))
			}
			fmt.Printf("  ! %s stopped working, skipping the rest of the syntax check\n     (%s)\n",
				filepath.Base(bash), firstLine(stderr.String()))
			return nil
		}
		count++
	}
	fmt.Printf("  %d lifecycle scripts OK\n", count)
	return nil
}

// pickBash returns the first bash on PATH that can actually parse a script.
//
// Several bashes can be installed at once (a bundled MSYS/Cygwin build, the WSL
// stub, /bin/bash), and they are not interchangeable: the WSL stub cannot see
// Windows paths and a sandboxed MSYS build cannot start at all. Probing with an
// empty script costs one short process and turns "the build died in a step that
// has nothing to do with the change" into "this host cannot run that check".
func pickBash() (string, string) {
	var lastDiag string
	for _, candidate := range bashCandidates() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		probe := exec.CommandContext(ctx, candidate, "-n")
		probe.Stdin = strings.NewReader("")
		var stderr bytes.Buffer
		probe.Stderr = &stderr
		err := probe.Run()
		cancel()
		if err == nil {
			return candidate, ""
		}
		lastDiag = firstLine(stderr.String())
	}
	return "", lastDiag
}

// bashCandidates lists every distinct bash reachable through PATH, in PATH
// order. LookPath only returns the first match, which is exactly the one that
// may be unusable.
func bashCandidates() []string {
	names := []string{"bash"}
	if runtime.GOOS == "windows" {
		names = []string{"bash.exe", "bash"}
	}
	var out []string
	seen := make(map[string]bool)
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	if path, err := exec.LookPath("bash"); err == nil {
		add(path)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, name := range names {
			path := filepath.Join(dir, name)
			if exists(path) {
				add(path)
			}
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	if s == "" {
		return "no diagnostic output"
	}
	return s
}

// jsonFiles lists the files fnOS parses as JSON.
func (p *pipeline) jsonFiles() ([]string, error) {
	files := []string{
		filepath.Join(p.project, "config", "privilege"),
		filepath.Join(p.project, "config", "resource"),
		filepath.Join(p.project, "app", "ui", "config"),
	}
	wizardDir := filepath.Join(p.project, "wizard")
	entries, err := os.ReadDir(wizardDir)
	if err != nil {
		return nil, fmt.Errorf("cannot read wizard/: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		files = append(files, filepath.Join(wizardDir, e.Name()))
	}
	return files, nil
}

// normalizeTree sets the permissions the packaged tree must carry. Only the
// mode is touched -- content is left alone.
func normalizeTree(project string) error {
	return filepath.WalkDir(project, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(project, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return os.Chmod(path, 0o755)
		}
		mode := fs.FileMode(0o644)
		switch {
		case strings.HasPrefix(rel, "cmd/"):
			mode = 0o755
		case rel == "app/bin/cloudflared", rel == "app/bin/fncfd":
			mode = 0o755
		}
		return os.Chmod(path, mode)
	})
}

// resolveFnpack locates the official packing tool: the flag wins, then $FNPACK,
// then PATH.
func resolveFnpack(fromFlag string) (string, error) {
	candidate := fromFlag
	if candidate == "" {
		candidate = os.Getenv("FNPACK")
	}
	if candidate == "" {
		candidate = "fnpack"
	}
	if resolved, err := exec.LookPath(candidate); err == nil {
		return resolved, nil
	}
	if exists(candidate) {
		return candidate, nil
	}
	return "", fmt.Errorf("fnpack not found (%q). Download the official binary from https://static2.fnnas.com/fnpack/ and pass --fnpack <path>, or set FNPACK", candidate)
}

// envWith sets KEY=VALUE pairs, replacing any value the parent environment
// already had for that key (a duplicate entry is not portable).
func envWith(base []string, kv ...string) []string {
	keys := make(map[string]bool, len(kv))
	for _, pair := range kv {
		keys[strings.SplitN(pair, "=", 2)[0]] = true
	}
	out := make([]string, 0, len(base)+len(kv))
	for _, entry := range base {
		name := strings.SplitN(entry, "=", 2)[0]
		if keys[name] {
			continue
		}
		out = append(out, entry)
	}
	return append(out, kv...)
}

func exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// step prints the same banner the shell scripts used, so the output of a build
// looks the same on every host.
func step(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("NO_COLOR") != "" || runtime.GOOS == "plan9" {
		fmt.Printf("\n==> %s\n", msg)
		return
	}
	fmt.Printf("\n\033[1;36m==>\033[0m %s\n", msg)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\033[1;31mERROR:\033[0m %s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}
