package config

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/ybd156/fnos-cloudflared/internal/fsx"
)

// ApplyWizardSettings merges the answers collected by the install/settings
// wizard (wizard_* environment variables) into config.json.
//
// This replaces an inline Node script the settings callback used to run: the
// package no longer depends on a Node runtime, so the one place that needed it
// to edit JSON now lives in the binary that owns the file.
//
// The read-modify-write is deliberate. Tunnel definitions and tokens are
// written by the service; this callback only owns the settings half, and must
// never clobber the other.
//
// The document is handled as a generic map so unknown top-level keys written by
// a newer version survive a round trip.
func ApplyWizardSettings(configFile string, getenv func(string) string) error {
	root := map[string]any{}
	if raw, err := os.ReadFile(configFile); err == nil {
		// A corrupt file is treated as empty, exactly like the previous
		// implementation: the wizard's job is to write a usable config, not to
		// refuse because the old one is broken.
		_ = json.Unmarshal(raw, &root)
		if root == nil {
			root = map[string]any{}
		}
	}

	settings, ok := root["settings"].(map[string]any)
	if !ok || settings == nil {
		settings = map[string]any{}
	}

	setBool := func(envName, key string) {
		if value, ok := parseWizardBool(getenv(envName)); ok {
			settings[key] = value
		}
	}
	setEnum := func(envName, key string, allowed []string) {
		value := strings.TrimSpace(getenv(envName))
		if value == "" {
			return
		}
		for _, candidate := range allowed {
			if value == candidate {
				settings[key] = value
				return
			}
		}
	}

	setBool("wizard_auto_start", "autoStart")
	setBool("wizard_auto_restart", "autoRestart")
	setEnum("wizard_log_level", "logLevel", []string{"debug", "info", "warn", "error"})
	setEnum("wizard_protocol", "protocol", []string{"auto", "quic", "http2"})

	root["settings"] = settings
	if _, exists := root["version"]; !exists {
		root["version"] = 1
	}

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsx.AtomicWrite(configFile, data, 0o600)
}

// parseWizardBool accepts the spellings the fnOS wizard can produce. An
// unrecognised value is reported as "not set" so it leaves the field alone.
func parseWizardBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}
