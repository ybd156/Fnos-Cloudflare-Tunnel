package fpk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var checksumLineRE = regexp.MustCompile(`(?m)^checksum\s*=.*$`)

// Normalize rewrites the permission bits of both tar layers inside an existing
// .fpk, then repairs the `checksum` line of the packaged manifest, which
// records the MD5 of app.tgz and therefore changes with it.
//
// This runs after `fnpack build`. fnpack derives archive modes from the host
// filesystem's stat(): on Linux the source tree has already been chmod'ed, but
// on Windows there is no executable bit at all, so cmd/* and bin/cloudflared
// would arrive on the NAS non-executable.
func Normalize(fpkPath string, out io.Writer) (ModeReport, error) {
	var report ModeReport

	raw, err := os.ReadFile(fpkPath)
	if err != nil {
		return report, err
	}
	outer, err := readTarGz(raw, "fpk")
	if err != nil {
		return report, err
	}
	outerIndex := index(outer)

	appMember, ok := outerIndex["app.tgz"]
	if !ok || appMember.isDir {
		return report, errors.New("not an fnpack archive (missing app.tgz)")
	}
	manifest, ok := outerIndex["manifest"]
	if !ok || manifest.isDir {
		return report, errors.New("not an fnpack archive (missing manifest)")
	}

	// --- inner layer: the app tree -------------------------------------
	inner, err := readTarGz(appMember.data, "app.tgz")
	if err != nil {
		return report, err
	}
	innerBefore := modes(inner)
	innerOut, err := writeTarGz(inner, func(m *member) int64 {
		return wantMode(cleanName(m.name), m.isDir)
	})
	if err != nil {
		return report, fmt.Errorf("app.tgz: %w", err)
	}
	report.Checksum = md5hex(innerOut)
	report.InnerChanged = countChanged(inner, innerBefore)
	appMember.data = innerOut

	// --- repair the manifest checksum ----------------------------------
	manifest.data = []byte(repairChecksum(string(manifest.data), report.Checksum))

	// --- outer layer ---------------------------------------------------
	outerBefore := modes(outer)
	outerOut, err := writeTarGz(outer, func(m *member) int64 {
		return wantMode(cleanName(m.name), m.isDir)
	})
	if err != nil {
		return report, err
	}
	report.OuterChanged = countChanged(outer, outerBefore)

	if err := os.WriteFile(fpkPath, outerOut, 0o644); err != nil {
		return report, err
	}
	if st, err := os.Stat(fpkPath); err == nil {
		report.Size = st.Size()
	}

	fmt.Fprintf(out, "  normalised %d outer + %d inner permission bits\n", report.OuterChanged, report.InnerChanged)
	fmt.Fprintf(out, "  checksum = %s (md5 of app.tgz)\n", report.Checksum)
	fmt.Fprintf(out, "  size     = %d bytes\n", report.Size)
	return report, nil
}

// repairChecksum replaces the checksum line, or appends one when fnpack did not
// write it (the field is added by fnpack, so an archive built by another tool
// may lack it).
func repairChecksum(manifest, sum string) string {
	if checksumLineRE.MatchString(manifest) {
		return checksumLineRE.ReplaceAllString(manifest, "checksum = "+sum)
	}
	return strings.TrimRight(manifest, "\n") + "\nchecksum = " + sum + "\n"
}

// modes snapshots the current mode of every member, keyed by name, so the
// report can say how many bits actually changed.
func modes(members []*member) map[string]int64 {
	m := make(map[string]int64, len(members))
	for _, mem := range members {
		m[mem.name] = mem.hdr.Mode
	}
	return m
}

func countChanged(members []*member, before map[string]int64) int {
	n := 0
	for _, m := range members {
		if want := wantMode(cleanName(m.name), m.isDir); before[m.name] != want {
			n++
		}
	}
	return n
}

// ReadManifest returns the packaged manifest as text.
func ReadManifest(fpkPath string) (string, error) {
	raw, err := os.ReadFile(fpkPath)
	if err != nil {
		return "", err
	}
	outer, err := readTarGz(raw, "fpk")
	if err != nil {
		return "", err
	}
	m, ok := index(outer)["manifest"]
	if !ok || m.isDir {
		return "", errors.New("not an fnpack archive (missing manifest)")
	}
	return string(m.data), nil
}
