package runner

import "testing"

func TestScanTransfersCountsBothKinds(t *testing.T) {
	lines := []string{
		`2026-11-07T06:40:38Z ERR  request failed error="Request Entity Too Large"`,
		`2026-11-07T06:41:02Z ERR  failed to copy body error="stream error: stream ID 13; INTERNAL_ERROR"`,
		`2026-11-07T06:41:03Z INF  Registered tunnel connection`,
		`2026-11-07T06:42:10Z ERR  read tcp: connection reset by peer`,
		`2026-11-07T06:43:00Z WRN  413 from origin`,
	}

	got := ScanTransfers(lines)
	if got.TooLarge != 2 {
		t.Errorf("TooLarge = %d, want 2", got.TooLarge)
	}
	if got.Reset != 2 {
		t.Errorf("Reset = %d, want 2", got.Reset)
	}
	if got.Interrupted != 4 {
		t.Errorf("Interrupted = %d, want 4", got.Interrupted)
	}
}

func TestScanTransfersIgnoresUnrelatedLines(t *testing.T) {
	lines := []string{
		`2026-11-07T06:40:38Z INF precheck component="DNS Resolution" status=pass`,
		`2026-11-07T06:40:38Z INF Registered tunnel connection connIndex=0`,
		``,
	}
	if got := ScanTransfers(lines); got != (TransferStats{}) {
		t.Fatalf("unexpected stats: %+v", got)
	}
}

// A 413 line must not also be counted as a reset: they are different failures
// with different advice, and double counting would show the wrong one first.
func TestScanTransfersIsExclusive(t *testing.T) {
	got := ScanTransfers([]string{`ERR 413 Request Entity Too Large`})
	if got.TooLarge != 1 || got.Reset != 0 || got.Interrupted != 1 {
		t.Fatalf("stats = %+v", got)
	}
}
