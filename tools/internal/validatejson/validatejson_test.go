package validatejson

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidFiles(t *testing.T) {
	a := write(t, "privilege", `{"defaults":{"run-as":"package"}}`)
	b := write(t, "resource", "{\n  \"data-share\": {}\n}\n")
	var out, errOut bytes.Buffer
	if err := Run([]string{a, b}, &out, &errOut); err != nil {
		t.Fatalf("unexpected failure: %v (%s)", err, errOut.String())
	}
}

func TestBOMIsTolerated(t *testing.T) {
	// The Windows editors in this project add a BOM; json.load() accepts it and
	// so must the build.
	path := write(t, "config", "\xef\xbb\xbf{\"entry\":\"cloudflared.main\"}")
	var out, errOut bytes.Buffer
	if err := Run([]string{path}, &out, &errOut); err != nil {
		t.Fatalf("a BOM-prefixed file was rejected: %v", err)
	}
}

func TestInvalidJSONFails(t *testing.T) {
	path := write(t, "config", `{"entry": "cloudflared.main",}`)
	var out, errOut bytes.Buffer
	if err := Run([]string{path}, &out, &errOut); err == nil {
		t.Fatal("a file with a trailing comma was accepted")
	}
	if errOut.Len() == 0 {
		t.Fatal("the failure was not reported")
	}
}

func TestTrailingContentFails(t *testing.T) {
	// Two concatenated objects: json.load() rejects this, and so must the gate.
	path := write(t, "config", `{"a":1}{"b":2}`)
	var out, errOut bytes.Buffer
	if err := Run([]string{path}, &out, &errOut); err == nil {
		t.Fatal("trailing content after the value was accepted")
	}
}

func TestMissingFileFails(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run([]string{filepath.Join(t.TempDir(), "nope")}, &out, &errOut); err == nil {
		t.Fatal("a missing file was accepted")
	}
}

func TestNoArgumentsIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run(nil, &out, &errOut); err == nil {
		t.Fatal("no arguments was accepted")
	}
}
