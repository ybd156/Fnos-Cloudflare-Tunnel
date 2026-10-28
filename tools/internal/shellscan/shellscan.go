// Package shellscan holds the textual scans shared by the shell linter and the
// artefact verifier.
//
// Both of them need to answer the same question about a shell script ("does
// this line invoke node/python?"), and both must answer it the same way: a
// gate that says "the package does not need Node" while the linter happily
// accepts a `node` call would be worse than no gate at all.
package shellscan

import "regexp"

// commentRE matches a trailing shell comment. A '#' only starts one at the
// beginning of a line or after whitespace, so `$#`, `a#b` and `#!/bin/sh` are
// left alone.
var commentRE = regexp.MustCompile(`(^|\s)#.*$`)

// StripComment removes a trailing comment so that prose mentioning node or an
// apostrophe in a comment is not mistaken for code.
func StripComment(line string) string {
	return commentRE.ReplaceAllString(line, "$1")
}

// interpreterRE finds a bare interpreter invocation. It deliberately requires a
// word boundary on both sides: `nodejs`, `node_modules` and `$NODE` must not
// match, because the first two are not invocations and the third is the
// resolved variable (which is exactly what the scripts are supposed to use).
var interpreterRE = regexp.MustCompile(`(^|[\s;|&(])(node|python3?|npm|pnpm)([\s;|&)]|$)`)

// FindInterpreter returns the interpreter a line invokes (node, python,
// python3, npm or pnpm), or "" when the line invokes none.
func FindInterpreter(line string) string {
	m := interpreterRE.FindStringSubmatch(StripComment(line))
	if m == nil {
		return ""
	}
	return m[2]
}

// ScanInterpreter reports every line of a script that invokes an interpreter,
// as 1-based line numbers paired with the interpreter name.
type Hit struct {
	Line int
	Name string
	Text string
}

// ScanInterpreter walks a script line by line.
func ScanInterpreter(script string) []Hit {
	var hits []Hit
	for i, line := range SplitLines(script) {
		if name := FindInterpreter(line); name != "" {
			hits = append(hits, Hit{Line: i + 1, Name: name, Text: line})
		}
	}
	return hits
}

// SplitLines splits on \n and drops a trailing \r, so a checkout with CRLF line
// endings scans identically to one with LF.
func SplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' {
			continue
		}
		out = append(out, trimCR(s[start:i]))
		start = i + 1
	}
	if start < len(s) {
		out = append(out, trimCR(s[start:]))
	}
	return out
}

func trimCR(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\r' {
		return s[:n-1]
	}
	return s
}
