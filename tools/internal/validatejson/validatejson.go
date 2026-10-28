// Package validatejson checks that the files fnOS parses as JSON really are
// valid JSON.
//
// fnOS reads config/privilege, config/resource, app/ui/config and the wizard
// descriptors as JSON at install time; a malformed one is rejected by the app
// centre, so the build checks them before packing.
//
// Port of tools/validate-json.py, including the two details Python's json.load
// gets right and a naive decoder does not: a UTF-8 BOM is tolerated (the
// Windows editors in this project add one), and trailing content after the
// value is an error rather than silently ignored.
package validatejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

var bom = []byte{0xEF, 0xBB, 0xBF}

// Run validates every path and reports the first failure of each.
func Run(files []string, out, errOut io.Writer) error {
	if len(files) == 0 {
		return errors.New("usage: validatejson <file> [<file> ...]")
	}

	bad := 0
	for _, path := range files {
		if err := validate(path); err != nil {
			fmt.Fprintf(errOut, "  INVALID %s: %v\n", path, err)
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d JSON files are invalid", bad, len(files))
	}
	fmt.Fprintf(out, "  %d JSON files OK\n", len(files))
	return nil
}

func validate(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	raw = bytes.TrimPrefix(raw, bom)

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return err
	}
	// json.load() rejects trailing data, and so must this: a file with two
	// concatenated objects would otherwise pass while fnOS sees something
	// different from what the author meant.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing content after the JSON value")
		}
		return fmt.Errorf("trailing content after the JSON value: %w", err)
	}
	return nil
}
