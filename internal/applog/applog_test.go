package applog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

/* ---------------------------------------------------------------- 尾部读取 */

func TestReadTailLinesReturnsLastLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	writeFile(t, path, "one\ntwo\nthree\nfour\n")

	lines := ReadTailLines(path, 64<<10, 2)
	if len(lines) != 2 || lines[0] != "three" || lines[1] != "four" {
		t.Errorf("应返回最后两行，得到 %v", lines)
	}
}

func TestReadTailLinesMissingOrEmpty(t *testing.T) {
	dir := t.TempDir()
	if got := ReadTailLines(filepath.Join(dir, "missing.log"), 1024, 10); got != nil {
		t.Errorf("文件不存在应返回 nil，得到 %v", got)
	}
	empty := filepath.Join(dir, "empty.log")
	writeFile(t, empty, "")
	if got := ReadTailLines(empty, 1024, 10); got != nil {
		t.Errorf("空文件应返回 nil，得到 %v", got)
	}
}

// TestReadTailLinesDropsPartialFirstLine covers the byte-window boundary: a
// window that starts mid-line must not emit the fragment as a log entry.
func TestReadTailLinesDropsPartialFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	// 33 bytes: "aaaaaaaaaa\n" (0-10) "bbbbbbbbbb\n" (11-21) "cccccccccc\n" (22-32)
	writeFile(t, path, "aaaaaaaaaa\nbbbbbbbbbb\ncccccccccc\n")

	// A 15-byte window starts at offset 18, i.e. inside the second line.
	lines := ReadTailLines(path, 15, 10)
	if len(lines) != 1 || lines[0] != "cccccccccc" {
		t.Errorf("应丢弃窗口边界上的残行，只返回完整行，得到 %v", lines)
	}
}

// TestReadTailLinesNeverReturnsUninitialisedBytes is the regression for the
// defect where a rotated (shrunk) log emitted whatever the read buffer happened
// to contain, because the whole buffer was decoded instead of only the bytes
// actually read.
func TestReadTailLinesNeverReturnsUninitialisedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	big := strings.Repeat("0123456789abcdef\n", 4096) // 72 KB
	writeFile(t, path, big)

	// Shrink it: this is what a rotation looks like to a reader that stat'ed
	// the file a moment earlier.
	if err := os.WriteFile(path, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lines := ReadTailLines(path, 64<<10, 100)
	for _, line := range lines {
		if strings.ContainsRune(line, 0) {
			t.Fatalf("返回了含 NUL 的日志行（缓冲区垃圾被当成正文）: %q", line)
		}
	}
	if len(lines) != 1 || lines[0] != "fresh" {
		t.Errorf("应只返回缩短后的内容，得到 %v", lines)
	}
}

func TestReadTailLinesRespectsMaxLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	writeFile(t, path, "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	lines := ReadTailLines(path, 64<<10, 3)
	if len(lines) != 3 || lines[2] != "10" {
		t.Errorf("应只返回最后 3 行，得到 %v", lines)
	}
}

/* ------------------------------------------------------------------ 轮转 */

func TestAppendPrivateRotatesOversizedLog(t *testing.T) {
	dir := t.TempDir()
	log := New(filepath.Join(dir, "server.log"))
	log.Out = nil
	log.Err = nil
	defer log.Close()

	path := filepath.Join(dir, "tunnel.log")
	writeFile(t, path, strings.Repeat("x", 2000))

	// The first append to a fresh sink runs the rotation check, so a log that
	// grew past its cap while the service was down is rotated immediately.
	log.AppendPrivate(path, "new line\n", 1000)

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("超限日志应被轮转到 .1: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new line\n" {
		t.Errorf("轮转后新日志应只含本次写入，得到 %q", string(data))
	}
}

func TestAppendPrivateDoesNotRotateSmallLog(t *testing.T) {
	dir := t.TempDir()
	log := New(filepath.Join(dir, "server.log"))
	log.Out = nil
	log.Err = nil
	defer log.Close()

	path := filepath.Join(dir, "tunnel.log")
	log.AppendPrivate(path, "line one\n", 1<<20)
	log.AppendPrivate(path, "line two\n", 1<<20)

	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("未超限的日志不应被轮转")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "line one\nline two\n" {
		t.Errorf("日志内容不符: %q", string(data))
	}
}

func TestAppendPrivateMode(t *testing.T) {
	dir := t.TempDir()
	log := New(filepath.Join(dir, "server.log"))
	log.Out = nil
	log.Err = nil
	defer log.Close()

	path := filepath.Join(dir, "tunnel.log")
	log.AppendPrivate(path, "x\n", 1<<20)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode != 0o640 {
		t.Errorf("隧道日志权限应为 0640，得到 %o", mode)
	}
}

/* ------------------------------------------------------------------ 格式 */

func TestLogLineFormat(t *testing.T) {
	dir := t.TempDir()
	appLog := filepath.Join(dir, "server.log")
	log := New(appLog)
	var out strings.Builder
	log.Out = &out
	log.Err = &out
	defer log.Close()

	log.Info("hello", 42)
	log.Error("boom")

	text := out.String()
	if !strings.Contains(text, "[INFO] hello 42") {
		t.Errorf("INFO 行格式不符: %q", text)
	}
	if !strings.Contains(text, "[ERROR] boom") {
		t.Errorf("ERROR 行格式不符: %q", text)
	}
	// The timestamp must be ISO-8601 UTC with milliseconds, matching what
	// existing logs and support tooling expect.
	first := strings.SplitN(text, "\n", 2)[0]
	if !strings.HasPrefix(first, "[") || !strings.HasSuffix(first, "Z] [INFO] hello 42") {
		t.Errorf("时间戳格式不符: %q", first)
	}

	// The same lines must land in the service log file.
	data, err := os.ReadFile(appLog)
	if err != nil {
		t.Fatalf("服务日志未写入: %v", err)
	}
	if !strings.Contains(string(data), "hello 42") || !strings.Contains(string(data), "boom") {
		t.Errorf("服务日志内容不符: %q", string(data))
	}
}

func TestRotateAppLogNow(t *testing.T) {
	dir := t.TempDir()
	appLog := filepath.Join(dir, "server.log")
	writeFile(t, appLog, strings.Repeat("y", 3<<20)) // over the 2MB cap

	log := New(appLog)
	log.Out = nil
	log.Err = nil
	defer log.Close()
	log.RotateAppLogNow()

	if _, err := os.Stat(appLog + ".1"); err != nil {
		t.Fatalf("超限的服务日志应在启动时被轮转: %v", err)
	}
}

func TestLoggerSurvivesUnwritablePath(t *testing.T) {
	dir := t.TempDir()
	log := New(filepath.Join(dir, "server.log"))
	log.Out = nil
	log.Err = nil
	defer log.Close()

	// A path whose parent cannot be created must not take the service down.
	log.AppendPrivate(filepath.Join(dir, "a\x00b", "x.log"), "line\n", 1024)
	log.Info("still alive")
}
