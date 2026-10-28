package fpk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ybd156/fnos-cloudflared/tools/internal/cfpin"
	"github.com/ybd156/fnos-cloudflared/tools/internal/shellscan"
)

// DefaultSource is the source directory the artefact is compared against.
const DefaultSource = "cloudflared"

// Outer-level members fnOS reads. cmd/ is checked separately (one entry per
// source script) and the manifest is compared field by field, not by name.
var outerRequired = []string{
	"app.tgz", "manifest", "ICON.PNG", "ICON_256.PNG",
	"config/privilege", "config/resource",
	"wizard/install", "wizard/config", "wizard/uninstall", "wizard/upgrade",
}

// Members inside app.tgz that the installed tree must have. Note there is no
// server/server.js any more: the payload runs a Go binary.
var appRequired = []string{
	"www/index.html", "www/style.css", "ui/config",
	"bin/cloudflared", "bin/fncfd",
	"config/privilege", "config/resource",
}

// notAuthored are the packaged files the source tree does not author:
//   - app/bin/cloudflared is the 38MB upstream build; it cannot be compared to
//     a source file, so it is compared to the pin in tools/internal/cfpin
//     instead (SHA-256 + the version string inside it);
//   - app/bin/fncfd is the compiled service, asserted separately as an ELF;
//   - manifest is realigned by fnpack and gains the checksum line, so
//     byte-identity can never hold for it.
var notAuthored = map[string]bool{
	"app/bin/cloudflared": true,
	"app/bin/fncfd":       true,
	"manifest":            true,
}

// VerifyOptions carries the thresholds and the pin. It is a value rather than
// constants so the tests can verify a small synthetic archive against a fake
// pin; the defaults are what the build uses.
type VerifyOptions struct {
	// MinBinaryBytes is the smallest plausible cloudflared. The real one is
	// ~38MB, so anything much smaller is a truncated download.
	MinBinaryBytes int64
	// MinServerBytes is the smallest plausible service binary. A stripped Go
	// build of this service is several MB; a few kilobytes means the build
	// step produced something that is not the service.
	MinServerBytes int64
	// MinAuthoredFiles guards against the source walk silently covering almost
	// nothing (an empty or wrong source directory).
	MinAuthoredFiles int
	// Pin is the upstream release the bundled binary must match.
	Pin cfpin.Pin
	// CheckPin disables the pin comparison (tests only).
	CheckPin bool
}

// DefaultVerifyOptions is what the build passes.
func DefaultVerifyOptions() VerifyOptions {
	return VerifyOptions{
		MinBinaryBytes:   30_000_000,
		MinServerBytes:   1_000_000,
		MinAuthoredFiles: 30,
		Pin:              cfpin.Pinned,
		CheckPin:         true,
	}
}

var (
	manifestKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	versionRE     = regexp.MustCompile(`(?m)^version\s*=\s*(\S+)`)
	checksumRE    = regexp.MustCompile(`(?m)^checksum\s*=\s*(\S+)`)
	scriptSrcRE   = regexp.MustCompile(`src="%%CF_BASE%%/([^"]+\.js)"`)
)

type checker struct {
	out      io.Writer
	passed   int
	failures []string
}

func (c *checker) section(title string) {
	fmt.Fprintf(c.out, "\n=== %s ===\n", title)
}

// ck records one assertion. detail is optional and is appended to the label
// when the check fails, so a failure says what was actually seen.
func (c *checker) ck(cond bool, label string, detail ...string) {
	if cond {
		c.passed++
		return
	}
	if len(detail) > 0 && detail[0] != "" {
		c.failures = append(c.failures, label+" ("+detail[0]+")")
	} else {
		c.failures = append(c.failures, label)
	}
}

// Verify re-opens a built .fpk and asserts what a bad build actually gets
// wrong, comparing it to the source tree at srcDir. It returns an error when
// any check failed.
func Verify(fpkPath, srcDir string, opt VerifyOptions, out io.Writer) error {
	c := &checker{out: out}

	raw, err := os.ReadFile(fpkPath)
	if err != nil {
		return err
	}

	// ---------------------------------------------------------- container
	c.section("container format")
	c.ck(len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b, "fpk is gzip-compressed")

	outer, err := readTarGz(raw, "fpk")
	if err != nil {
		c.ck(false, "the .fpk is a readable gzip'd tar", err.Error())
		return c.finish()
	}
	outerIndex := index(outer)
	outerNames := memberNames(outer)
	fmt.Fprintf(out, "  outer members: %d\n", len(outerNames))

	for _, req := range outerRequired {
		c.ck(outerIndex[req] != nil, "outer contains "+req)
	}

	var cmdMembers []string
	for _, name := range outerNames {
		if strings.HasPrefix(name, "cmd/") {
			cmdMembers = append(cmdMembers, cleanName(name))
		}
	}
	c.ck(outerIndex["cmd"] != nil || len(cmdMembers) > 0, "outer contains cmd/")
	c.ck(outerIndex["cmd/main"] != nil, "outer contains cmd/main")

	// The count is derived from the source tree rather than hard-coded: a
	// script dropped by the packer and a script added to the tree must both be
	// noticed, and the byte-for-byte walk below covers the contents.
	srcCmd, err := listDir(filepath.Join(srcDir, "cmd"))
	if err != nil {
		c.ck(false, "source cmd/ directory is readable", err.Error())
	} else {
		c.ck(len(cmdMembers) == len(srcCmd), "cmd/ carries one entry per source script",
			fmt.Sprintf("packed=%d source=%d", len(cmdMembers), len(srcCmd)))
	}
	fmt.Fprintf(out, "  cmd/ members: %d\n", len(cmdMembers))

	// ----------------------------------------------------------- manifest
	c.section("manifest")
	packedManifest := ""
	if m := outerIndex["manifest"]; m != nil && !m.isDir {
		packedManifest = string(m.data)
	}
	pkgFields, pkgStray := parseManifest(packedManifest)

	packedVer := ""
	if m := versionRE.FindStringSubmatch(packedManifest); m != nil {
		packedVer = m[1]
	}
	c.ck(packedVer != "", "manifest declares version")
	if packedVer != "" {
		fmt.Fprintf(out, "  version = %s\n", packedVer)
	}

	srcManifestPath := filepath.Join(srcDir, "manifest")
	if !fileExists(srcManifestPath) {
		c.ck(false, "source manifest found", srcManifestPath)
	} else {
		srcBytes, err := os.ReadFile(srcManifestPath)
		if err != nil {
			c.ck(false, "source manifest is readable", err.Error())
		} else {
			srcFields, srcStray := parseManifest(string(srcBytes))

			// fnpack does NOT copy the manifest verbatim: it realigns the '='
			// padding and appends the checksum line. Compare field by field --
			// that is the invariant fnOS actually reads, and it still catches a
			// stale or truncated manifest.
			c.ck(len(srcStray) == 0, "source manifest has no malformed lines", joinLimit(srcStray, 3))

			var mismatched []string
			for _, key := range sortedKeys(srcFields) {
				want := srcFields[key]
				got, ok := pkgFields[key]
				switch {
				case !ok:
					mismatched = append(mismatched, key+": missing from the package")
				case got != want:
					mismatched = append(mismatched,
						fmt.Sprintf("%s: packed %d chars != source %d chars", key, len(got), len(want)))
				}
			}
			c.ck(len(mismatched) == 0, "every manifest field matches the source manifest", joinLimit(mismatched, 4))

			// fnpack splits a value at a ';' and prefixes the continuation with
			// ';', which is an INI COMMENT -- the rest of the value is then
			// silently dropped by fnOS. A stray line here means that happened.
			c.ck(len(pkgStray) == 0,
				"packaged manifest has no stray lines (a ';' in a value is mangled)",
				joinLimit(pkgStray, 2))

			c.ck(srcFields["version"] == packedVer, "packaged version matches the source manifest",
				fmt.Sprintf("packed=%s source=%s", packedVer, srcFields["version"]))
			c.ck(srcFields["version"] != "" && strings.Contains(srcFields["changelog"], srcFields["version"]),
				"changelog names the declared version", srcFields["version"])
		}
	}

	appMember := outerIndex["app.tgz"]
	var appBytes []byte
	if appMember != nil && !appMember.isDir {
		appBytes = appMember.data
	}
	appMD5 := md5hex(appBytes)
	declared := ""
	if m := checksumRE.FindStringSubmatch(packedManifest); m != nil {
		declared = m[1]
	}
	c.ck(declared != "", "manifest has checksum line")
	if declared != "" {
		c.ck(appMD5 == declared, "checksum == md5(app.tgz)",
			fmt.Sprintf("calc=%s declared=%s", appMD5, declared))
	}
	fmt.Fprintf(out, "  md5(app.tgz) = %s\n", appMD5)

	// ------------------------------------------------------------ app.tgz
	c.section("app.tgz contents")
	inner, err := readTarGz(appBytes, "app.tgz")
	if err != nil {
		c.ck(false, "app.tgz is a readable gzip'd tar", err.Error())
		return c.finish()
	}
	innerIndex := index(inner)
	innerNames := memberNames(inner)
	fmt.Fprintf(out, "  app.tgz members: %d\n", len(innerNames))
	for _, req := range appRequired {
		c.ck(innerIndex[req] != nil, "app.tgz contains "+req)
	}

	outerBytes := bodies(outer)
	innerBytes := bodies(inner)

	// ------------------------------------------- refactor invariants (Go era)
	// These replace the 1.3.1/1.4.0/1.4.1 Node probes: those asserted that
	// particular lines existed in server/lib/*.js, which no longer ship. The
	// property they protected is still worth a gate -- a package must not
	// quietly drag Node back in, and the payload must really contain the Go
	// server -- so the probes now read the artefact instead of the sources.
	c.section("refactor invariants (no Node, Go server)")

	var interpreterHits []string
	for _, name := range cmdMembers {
		m := outerIndex[name]
		if m == nil {
			continue
		}
		for _, hit := range shellscan.ScanInterpreter(string(m.data)) {
			interpreterHits = append(interpreterHits,
				fmt.Sprintf("%s:%d %s", name, hit.Line, hit.Name))
		}
	}
	c.ck(len(interpreterHits) == 0,
		"packaged cmd/* scripts do not invoke node/python", joinLimit(interpreterHits, 4))

	dep := pkgFields["install_dep_apps"]
	c.ck(!strings.Contains(strings.ToLower(dep), "node"),
		"manifest declares no Node runtime dependency", "install_dep_apps = "+dep)

	var nodeLeftovers []string
	for _, name := range innerNames {
		if name == "server" || strings.HasPrefix(name, "server/") {
			nodeLeftovers = append(nodeLeftovers, name)
		}
	}
	c.ck(len(nodeLeftovers) == 0, "app.tgz no longer ships the Node backend (server/)",
		joinLimit(nodeLeftovers, 4))

	fncfd := innerBytes["bin/fncfd"]
	c.ck(int64(len(fncfd)) > opt.MinServerBytes, "the Go server binary is present", fmt.Sprintf("%d bytes", len(fncfd)))
	c.ck(hasPrefix(fncfd, []byte("\x7fELF")), "bin/fncfd is an ELF executable", magicOf(fncfd))
	if int64(len(fncfd)) > opt.MinServerBytes {
		fmt.Fprintf(out, "  bin/fncfd size = %d bytes\n", len(fncfd))
	}

	if mainScript := outerBytes["cmd/main"]; mainScript != nil {
		c.ck(strings.Contains(string(mainScript), "bin/fncfd"),
			"cmd/main launches the Go server (bin/fncfd)")
	} else {
		c.ck(false, "packaged cmd/main is present")
	}

	// The uninstall must not leave rotated log generations or version markers
	// behind when the user chose to delete their data.
	if un := outerBytes["cmd/uninstall_callback"]; un == nil {
		c.ck(false, "packaged cmd/uninstall_callback is present")
	} else {
		for _, leftover := range []string{"server.log.1", "VERSION", "cloudflared.version"} {
			c.ck(strings.Contains(string(un), leftover), "uninstall removes "+leftover)
		}
	}

	// ------------------------------------------------ frontend contract
	c.section("frontend contract")
	idxSrc := string(innerBytes["www/index.html"])
	c.ck(strings.Contains(idxSrc, "%%CF_BASE%%"),
		"index.html still carries the %%CF_BASE%% placeholder (substituted at serve time)")
	for _, landmark := range []string{"view-tunnels", "tunnel-summary", "modal-backdrop", "skip-link"} {
		c.ck(strings.Contains(idxSrc, landmark), "index.html contains "+landmark)
	}

	// The UI is plain ES modules with no build step, so a wrong script path is
	// a silently dead page: nothing resolves it at build time. Check that every
	// module index.html loads really is in the package.
	scriptSrcs := scriptSrcRE.FindAllStringSubmatch(idxSrc, -1)
	c.ck(len(scriptSrcs) > 0, "index.html loads at least one module")
	for _, m := range scriptSrcs {
		c.ck(innerIndex["www/"+m[1]] != nil, "index.html module "+m[1]+" is in the package")
	}

	var jsFiles []string
	var jsAll strings.Builder
	for _, name := range innerNames {
		if strings.HasPrefix(name, "www/") && strings.HasSuffix(name, ".js") {
			jsFiles = append(jsFiles, name)
			jsAll.WriteString(string(innerBytes[name]))
			jsAll.WriteString("\n")
		}
	}
	c.ck(len(jsFiles) >= 1, "the payload ships its frontend modules", fmt.Sprintf("%d .js files", len(jsFiles)))
	js := jsAll.String()
	// Polling must stop in a background tab: a hidden tab that keeps polling
	// burns the NAS's CPU for nothing, which is why 1.4.0 added this.
	c.ck(strings.Contains(js, "visibilitychange"), "the frontend pauses polling in a background tab")
	c.ck(!strings.Contains(js, "MARKER"), "no probe marker leaked into the frontend")

	cssSrc := string(innerBytes["www/style.css"])
	c.ck(strings.Contains(cssSrc, "prefers-color-scheme"), "style.css supports a dark theme")
	c.ck(strings.Contains(cssSrc, "prefers-reduced-motion"), "style.css respects reduced motion")
	c.ck(strings.Contains(cssSrc, "--primary:"), "style.css is driven by design tokens")

	// ------------------------------------------------- byte-for-byte check
	// Every authored file, in whichever layer it lives: app/* is inside
	// app.tgz, everything else (cmd/, wizard/, config/, manifest, icons) is at
	// the outer level.
	c.section("packaged files match source")
	pairs, err := discoverAuthored(srcDir)
	if err != nil {
		c.ck(false, "source tree is walkable", err.Error())
	}

	compared := 0
	var missing []string
	for _, pair := range pairs {
		disk, err := os.ReadFile(pair.disk)
		if err != nil {
			c.ck(false, "source file is readable: "+pair.disk, err.Error())
			continue
		}
		var packed []byte
		var ok bool
		if strings.HasPrefix(pair.rel, "app/") {
			packed, ok = innerBytes[pair.arc]
		} else {
			packed, ok = outerBytes[pair.arc]
		}
		if !ok {
			missing = append(missing, pair.arc)
			c.ck(false, "packaged file missing: "+pair.arc)
			continue
		}
		c.ck(bytesEqual(packed, disk), pair.arc+" matches source",
			fmt.Sprintf("packed=%d disk=%d", len(packed), len(disk)))
		compared++
	}

	fmt.Fprintf(out, "  compared %d authored files (discovered, not hand-listed)\n", compared)
	if len(missing) > 0 {
		fmt.Fprintf(out, "    MISSING from package: %s\n", joinLimit(missing, 6))
	}

	// Guard against the walk silently covering almost nothing: the tree really
	// does have this many authored files, and the lifecycle scripts and wizard
	// descriptors -- the ones fnOS executes -- must be among them.
	c.ck(compared >= opt.MinAuthoredFiles, "a substantial number of authored files was compared",
		fmt.Sprintf("only %d", compared))

	arcs := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		arcs[p.arc] = true
	}
	musts := []string{"cmd/main", "wizard/install", "wizard/config", "config/privilege", "ICON.PNG"}
	if fileExists(filepath.Join(srcDir, "cmd", "_lib")) {
		musts = append(musts, "cmd/_lib")
	}
	for _, must := range musts {
		c.ck(arcs[must], "authored walk includes "+must)
	}

	// -------------------------------------------------------- permissions
	c.section("archive permissions")
	var bad []string
	for _, m := range outer {
		if want := wantMode(cleanName(m.name), m.isDir); m.hdr.Mode != want {
			bad = append(bad, fmt.Sprintf("outer:%s %s!=%s", cleanName(m.name), octal(m.hdr.Mode), octal(want)))
		}
	}
	for _, m := range inner {
		if want := wantMode(cleanName(m.name), m.isDir); m.hdr.Mode != want {
			bad = append(bad, fmt.Sprintf("app:%s %s!=%s", cleanName(m.name), octal(m.hdr.Mode), octal(want)))
		}
	}

	outerModes := modeMap(outer)
	innerModes := modeMap(inner)
	c.ck(innerModes["bin/cloudflared"] == execMode, "bin/cloudflared is 0755", octal(innerModes["bin/cloudflared"]))
	c.ck(innerModes["bin/fncfd"] == execMode, "bin/fncfd is 0755", octal(innerModes["bin/fncfd"]))
	c.ck(outerModes["cmd/main"] == execMode, "cmd/main is 0755", octal(outerModes["cmd/main"]))
	for _, name := range cmdMembers {
		c.ck(outerModes[name] == execMode, name+" is 0755", octal(outerModes[name]))
	}
	c.ck(len(bad) == 0, "no unexpected 0666/0777 modes", joinLimit(bad, 4))
	fmt.Fprintf(out, "  cmd/main        = %s\n", octal(outerModes["cmd/main"]))
	fmt.Fprintf(out, "  bin/cloudflared = %s\n", octal(innerModes["bin/cloudflared"]))
	for i, b := range bad {
		if i >= 10 {
			break
		}
		fmt.Fprintf(out, "    BAD %s\n", b)
	}

	// ------------------------------------------------------------- binary
	// The binary is not tracked in git (38MB of upstream build), so it cannot be
	// compared to a source file. It is compared to the pin instead -- which is
	// what the build actually downloaded, and therefore the only thing that can
	// catch a stale or swapped binary.
	c.section("bundled binary")
	binBytes := innerBytes["bin/cloudflared"]
	c.ck(int64(len(binBytes)) > opt.MinBinaryBytes, "cloudflared binary is present",
		fmt.Sprintf("%d bytes", len(binBytes)))
	c.ck(hasPrefix(binBytes, []byte("\x7fELF")), "binary is an ELF executable", magicOf(binBytes))
	fmt.Fprintf(out, "  size = %d bytes, magic = %s\n", len(binBytes), magicOf(binBytes))

	if opt.CheckPin {
		if opt.Pin.Version == "" || opt.Pin.SHA256 == "" {
			c.ck(false, "the pinned binary version and SHA-256 are declared")
		} else {
			fmt.Fprintf(out, "  pinned %s sha256 %s...\n", opt.Pin.Version, short8(opt.Pin.SHA256))
			// The pin must describe *this* binary. The hash alone would not
			// catch a version string bumped without re-deriving the hash, and
			// the version alone would not catch a swapped same-version rebuild,
			// so assert both.
			c.ck(strings.Contains(string(binBytes), opt.Pin.Version),
				"the pinned version string really occurs in the packaged binary",
				"pin="+opt.Pin.Version)
			sum := sha256.Sum256(binBytes)
			actual := hex.EncodeToString(sum[:])
			c.ck(actual == opt.Pin.SHA256, "packaged binary matches the pinned SHA-256",
				fmt.Sprintf("packed=%s... pin=%s...", short8(actual), short8(opt.Pin.SHA256)))
			fmt.Fprintf(out, "  packaged sha256 %s\n", actual)
		}
	}

	return c.finish()
}

// finish prints the recap and turns any failure into an error.
func (c *checker) finish() error {
	fmt.Fprintln(c.out)
	fmt.Fprintln(c.out, strings.Repeat("=", 60))
	fmt.Fprintf(c.out, "RESULT: %d passed, %d failed\n", c.passed, len(c.failures))
	if len(c.failures) > 0 {
		fmt.Fprintln(c.out, "FAILED:")
		for _, f := range c.failures {
			fmt.Fprintln(c.out, "  -", f)
		}
		return fmt.Errorf("%d artefact check(s) failed", len(c.failures))
	}
	fmt.Fprintln(c.out, "ALL CHECKS PASSED")
	return nil
}

// Summary reports the artefact's size, SHA-256 and manifest checksum.
func Summary(fpkPath string) (Info, error) {
	var info Info
	raw, err := os.ReadFile(fpkPath)
	if err != nil {
		return info, err
	}
	info.Size = int64(len(raw))
	sum := sha256.Sum256(raw)
	info.SHA256 = hex.EncodeToString(sum[:])
	manifest, err := ReadManifest(fpkPath)
	if err != nil {
		return info, err
	}
	if m := checksumRE.FindStringSubmatch(manifest); m != nil {
		info.ManifestChecksum = m[1]
	}
	return info, nil
}

// ---------------------------------------------------------------- helpers

type authoredFile struct {
	arc  string // path inside the layer that holds it
	rel  string // path relative to the source directory
	disk string // absolute path on disk
}

// discoverAuthored walks the source tree and returns every file the package
// should contain.
//
// This used to be a hand-maintained list of 13 files, which meant the files
// fnOS actually executes and renders -- all of cmd/*, the wizard descriptors,
// config/*, both icons and the manifest -- were never compared, and a stale
// lifecycle script shipped with a green gate. Walking the tree means a new file
// is covered by default instead of being silently omitted.
func discoverAuthored(srcDir string) ([]authoredFile, error) {
	var out []authoredFile
	err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if notAuthored[rel] {
			return nil
		}
		arc := rel
		if strings.HasPrefix(rel, "app/") {
			// The packer puts cmd/, wizard/, config/, manifest and ICON*.PNG at
			// the outer level; everything under app/ goes inside app.tgz.
			arc = strings.TrimPrefix(rel, "app/")
		}
		out = append(out, authoredFile{arc: arc, rel: rel, disk: p})
		return nil
	})
	return out, err
}

// parseManifest returns (key -> value, [lines that are not well-formed
// key=value]) so a value mangled by fnpack's ';' handling shows up as a stray
// line rather than as a silently missing field.
func parseManifest(text string) (map[string]string, []string) {
	fields := make(map[string]string)
	var stray []string
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			stray = append(stray, line)
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if !manifestKeyRE.MatchString(key) {
			stray = append(stray, line)
			continue
		}
		fields[key] = strings.TrimSpace(line[idx+1:])
	}
	return fields, stray
}

func memberNames(members []*member) []string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, cleanName(m.name))
	}
	return names
}

func bodies(members []*member) map[string][]byte {
	m := make(map[string][]byte, len(members))
	for _, mem := range members {
		if !mem.isDir {
			m[cleanName(mem.name)] = mem.data
		}
	}
	return m
}

func modeMap(members []*member) map[string]int64 {
	m := make(map[string]int64, len(members))
	for _, mem := range members {
		m[cleanName(mem.name)] = mem.hdr.Mode
	}
	return m
}

func listDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func joinLimit(items []string, limit int) string {
	if len(items) > limit {
		items = items[:limit]
	}
	return strings.Join(items, "; ")
}

// octal formats a permission bit set the way the shell reports it (0755).
func octal(mode int64) string {
	return "0" + strconv.FormatInt(mode, 8)
}

func magicOf(b []byte) string {
	if len(b) < 4 {
		return hex.EncodeToString(b)
	}
	return hex.EncodeToString(b[:4])
}

func hasPrefix(b, prefix []byte) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == string(prefix)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func short8(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}
