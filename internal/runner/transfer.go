package runner

import "regexp"

// TransferStats counts the ways a request body can die mid-flight, over the
// recent window of one tunnel's log.
//
// The two causes need different fixes -- a request-body limit is a plan or
// origin limit that the app cannot lift, a reset stream is a transport problem
// that switching to HTTP/2 usually fixes -- so they are counted apart instead
// of being lumped into "断流".
type TransferStats struct {
	Interrupted int `json:"interrupted"`
	TooLarge    int `json:"tooLarge"`
	Reset       int `json:"reset"`
}

// transferWindow is how many recent lines are examined. The ring holds 600, and
// an upload failure usually shows up within a handful of lines of the request.
const transferWindow = 400

var (
	// The edge (or an origin reverse proxy) rejected the body outright.
	tooLargeRE = regexp.MustCompile(`(?i)Request Entity Too Large|\b413\b`)
	// The body was cut off after it started.
	streamResetRE = regexp.MustCompile(
		`(?i)stream (?:error|closed|reset)|unexpected EOF|connection reset by peer|broken pipe`)
)

// ScanTransfers classifies the given log lines.
func ScanTransfers(lines []string) TransferStats {
	var s TransferStats
	for _, line := range lines {
		switch {
		case tooLargeRE.MatchString(line):
			s.TooLarge++
			s.Interrupted++
		case streamResetRE.MatchString(line):
			s.Reset++
			s.Interrupted++
		}
	}
	return s
}
