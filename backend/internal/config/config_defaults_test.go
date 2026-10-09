package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultFingerprintArgsIncludeEffectiveRuntimeArgs(t *testing.T) {
	args := defaultFingerprintArgsForOS("windows")
	assertStringSliceContains(t, args, "--fingerprint-brand=Chrome")
	assertStringSliceContains(t, args, "--fingerprint-platform=windows")
	assertStringSliceContains(t, args, "--disable-non-proxied-udp")
	assertStringSliceContains(t, args, "--fingerprinting-canvas-image-data-noise")
	assertStringSliceContains(t, args, "--fingerprinting-client-rects-noise")
}

func TestNormalizeConfigUpgradesLegacyMinimalDefaultFingerprintArgs(t *testing.T) {
	config := &Config{}
	config.Browser.DefaultFingerprintArgs = []string{"--fingerprint-brand=Chrome", "--fingerprint-platform=windows"}

	normalizeConfig(config)

	assertStringSliceContains(t, config.Browser.DefaultFingerprintArgs, "--fingerprint-brand=Chrome")
	assertStringSliceContains(t, config.Browser.DefaultFingerprintArgs, "--fingerprint-platform=windows")
	assertStringSliceContains(t, config.Browser.DefaultFingerprintArgs, "--disable-non-proxied-udp")
	assertStringSliceContains(t, config.Browser.DefaultFingerprintArgs, "--fingerprinting-canvas-image-data-noise")
	assertStringSliceContains(t, config.Browser.DefaultFingerprintArgs, "--fingerprinting-client-rects-noise")
}

func TestNormalizeConfigDoesNotOverrideCustomDefaultFingerprintArgs(t *testing.T) {
	config := &Config{}
	config.Browser.DefaultFingerprintArgs = []string{"--fingerprint=123", "--fingerprint-brand=Chrome"}

	normalizeConfig(config)

	if got, want := len(config.Browser.DefaultFingerprintArgs), 2; got != want {
		t.Fatalf("default fingerprint args length = %d, want %d: %#v", got, want, config.Browser.DefaultFingerprintArgs)
	}
	assertStringSliceContains(t, config.Browser.DefaultFingerprintArgs, "--fingerprint=123")
	assertStringSliceContains(t, config.Browser.DefaultFingerprintArgs, "--fingerprint-brand=Chrome")
}

func assertStringSliceContains(t *testing.T, values []string, expected string) {
	t.Helper()
	for _, value := range values {
		if value == expected {
			return
		}
	}
	t.Fatalf("values %#v missing %q", values, expected)
}

func TestLoadKeepsMCPAllowedHostsAcrossSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mcp:\n    enabled: true\n    path: /mcp\n    allowed_hosts:\n        - 10.192.168.31\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.MCP.AllowedHosts; len(got) != 1 || got[0] != "10.192.168.31" {
		t.Fatalf("allowed_hosts not loaded: %v", got)
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.MCP.AllowedHosts; len(got) != 1 || got[0] != "10.192.168.31" {
		t.Fatalf("allowed_hosts lost after save: %v", got)
	}
}

func TestNormalizeKeepsAllowedHostsOnDisabledMCP(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MCP = MCPConfig{Enabled: false, AllowedHosts: []string{"10.192.168.31"}}

	normalizeConfig(cfg)

	if cfg.MCP.Enabled {
		t.Fatal("a disabled MCP section that only sets allowed_hosts must not be reset to defaults (which enable MCP)")
	}
	if len(cfg.MCP.AllowedHosts) != 1 {
		t.Fatalf("allowed_hosts dropped: %v", cfg.MCP.AllowedHosts)
	}
}
