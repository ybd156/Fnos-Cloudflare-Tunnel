package runner

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

func TestRingKeepsNewestLines(t *testing.T) {
	h := newHarness(t, testTunnel("a"))

	total := paths.MaxLogLines + 100
	for i := 0; i < total; i++ {
		h.m.pushLog("a", "line "+strconv.Itoa(i))
	}

	lines := h.m.Logs("a", paths.MaxLogLines)
	if len(lines) != paths.MaxLogLines {
		t.Fatalf("kept %d lines, want %d", len(lines), paths.MaxLogLines)
	}
	if !strings.HasSuffix(lines[0], "line "+strconv.Itoa(total-paths.MaxLogLines)) {
		t.Fatalf("oldest kept line = %q, want line %d", lines[0], total-paths.MaxLogLines)
	}
	if !strings.HasSuffix(lines[len(lines)-1], "line "+strconv.Itoa(total-1)) {
		t.Fatalf("newest kept line = %q, want line %d", lines[len(lines)-1], total-1)
	}
}

func TestLogsHonoursLimit(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	for i := 0; i < 10; i++ {
		h.m.pushLog("a", "line "+strconv.Itoa(i))
	}

	lines := h.m.Logs("a", 3)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if !strings.HasSuffix(lines[0], "line 7") || !strings.HasSuffix(lines[2], "line 9") {
		t.Fatalf("wrong window: %v", lines)
	}

	// A limit above the cap is clamped rather than rejected.
	if got := len(h.m.Logs("a", paths.MaxLogLines*10)); got != 10 {
		t.Fatalf("got %d lines, want all 10", got)
	}
	// A zero limit falls back to the endpoint default.
	if got := len(h.m.Logs("a", 0)); got != 10 {
		t.Fatalf("got %d lines, want all 10", got)
	}
}

func TestLogsUnknownTunnelLeavesNoRecord(t *testing.T) {
	h := newHarness(t)

	if lines := h.m.Logs("nope", 10); lines != nil {
		t.Fatalf("Logs(unknown) = %v, want nil", lines)
	}
	if h.hasRecord("nope") {
		t.Fatal("Logs for an unknown id must not create a runtime record (invariant 10)")
	}
}

func TestLogsFallBackToTheFileTail(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.m.pushLog("a", "first\nsecond")

	// Forget drops the in-memory ring, as if the service had just restarted.
	h.m.Forget("a")

	lines := h.m.Logs("a", 10)
	if len(lines) != 2 {
		t.Fatalf("got %d lines from the file, want 2: %v", len(lines), lines)
	}
	if !strings.HasSuffix(lines[0], "first") || !strings.HasSuffix(lines[1], "second") {
		t.Fatalf("file tail = %v", lines)
	}
}

func TestPushLogAddsUTCTimePrefix(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.m.pushLog("a", "hello")

	lines := h.m.Logs("a", 1)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	entry := lines[0]
	if len(entry) < 11 || entry[0] != '[' || entry[9] != ']' {
		t.Fatalf("entry %q must look like [HH:MM:SS] text", entry)
	}
	stamp := entry[1:9]
	if _, err := time.Parse("15:04:05", stamp); err != nil {
		t.Fatalf("entry prefix %q is not HH:MM:SS: %v", stamp, err)
	}
	// The stamp is UTC: compare against the UTC clock, not the local one.
	now := time.Now().UTC().Format("15:04:05")
	if stamp != now && stamp[:5] != now[:5] {
		t.Fatalf("entry stamp %q does not look like UTC %q", stamp, now)
	}
}

func TestPushLogSplitsChunksAndDropsCarriageReturns(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.m.pushLog("a", "one\r\ntwo\nthree\n")

	lines := h.m.Logs("a", 10)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), lines)
	}
	for _, line := range lines {
		if strings.Contains(line, "\r") {
			t.Fatalf("line still carries a carriage return: %q", line)
		}
	}
	if !strings.HasSuffix(lines[2], "three") {
		t.Fatalf("last line = %q", lines[2])
	}
}

func TestPushLogCapturesQuickTunnelURL(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.m.pushLog("a", "INF |  https://blue-lamp-tree.trycloudflare.com  |")

	var got string
	h.withRec("a", func(r *rec) { got = r.url })
	if got != "https://blue-lamp-tree.trycloudflare.com" {
		t.Fatalf("url = %q", got)
	}

	// An unrelated host must not be mistaken for the tunnel URL.
	h.m.pushLog("a", "https://example.com/not-a-quick-tunnel")
	h.withRec("a", func(r *rec) { got = r.url })
	if got != "https://blue-lamp-tree.trycloudflare.com" {
		t.Fatalf("url changed to %q", got)
	}
}

func TestPushLogPromotesStartingToRunning(t *testing.T) {
	cases := []string{
		"INF Registered tunnel connection connIndex=0",
		"INF Connection 1234abcd-1234-1234-1234-1234abcd5678 registered",
		"INF Initial protocol quic",
	}
	for _, line := range cases {
		h := newHarness(t, testTunnel("a"))
		h.withRec("a", func(r *rec) { r.status = StatusStarting })
		h.m.pushLog("a", line)
		if got := h.status("a"); got != StatusRunning {
			t.Fatalf("%q: status = %q, want %q", line, got, StatusRunning)
		}
		var errMsg string
		h.withRec("a", func(r *rec) { errMsg = r.errMsg })
		if errMsg != "" {
			t.Fatalf("%q: error should be cleared, got %q", line, errMsg)
		}
	}
}

func TestPushLogSamplesErrorsWhileStarting(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.withRec("a", func(r *rec) { r.status = StatusStarting })
	h.m.pushLog("a", "ERR failed to connect to the edge")

	var errMsg string
	h.withRec("a", func(r *rec) { errMsg = r.errMsg })
	if errMsg != "ERR failed to connect to the edge" {
		t.Fatalf("error = %q", errMsg)
	}

	// Once the tunnel is running the sampled error is no longer interesting.
	h.withRec("a", func(r *rec) {
		r.status = StatusRunning
		r.errMsg = ""
	})
	h.m.pushLog("a", "ERR transient")
	h.withRec("a", func(r *rec) { errMsg = r.errMsg })
	if errMsg != "" {
		t.Fatalf("a running tunnel must not sample errors, got %q", errMsg)
	}
}

func TestPushLogTruncatesLongError(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	h.withRec("a", func(r *rec) { r.status = StatusStarting })

	long := "ERR " + strings.Repeat("错误", 400)
	h.m.pushLog("a", long)

	var errMsg string
	h.withRec("a", func(r *rec) { errMsg = r.errMsg })
	if runes := []rune(errMsg); len(runes) != errorSampleRunes {
		t.Fatalf("sampled %d runes, want %d", len(runes), errorSampleRunes)
	}
}

func TestPushLogErrorIsCaseInsensitive(t *testing.T) {
	for _, line := range []string{"unable to reach origin", "Failed to dial", "level=error msg=boom"} {
		h := newHarness(t, testTunnel("a"))
		h.withRec("a", func(r *rec) { r.status = StatusStarting })
		h.m.pushLog("a", line)
		var errMsg string
		h.withRec("a", func(r *rec) { errMsg = r.errMsg })
		if errMsg == "" {
			t.Fatalf("%q should have been sampled as an error", line)
		}
	}
}

func TestRingReset(t *testing.T) {
	var r ring
	r.push("a")
	r.push("b")
	r.reset()
	if got := r.tail(10); got != nil {
		t.Fatalf("tail after reset = %v, want nil", got)
	}
	r.push("c")
	if got := r.tail(10); len(got) != 1 || got[0] != "c" {
		t.Fatalf("tail = %v, want [c]", got)
	}
}

func TestRingWrapsCorrectly(t *testing.T) {
	var r ring
	for i := 0; i < paths.MaxLogLines+5; i++ {
		r.push(strconv.Itoa(i))
	}
	got := r.tail(3)
	want := []string{strconv.Itoa(paths.MaxLogLines + 2), strconv.Itoa(paths.MaxLogLines + 3), strconv.Itoa(paths.MaxLogLines + 4)}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tail(3) = %v, want %v", got, want)
		}
	}
}
