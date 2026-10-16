package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.toml")
	content := `
# Global defaults
[defaults]
local_prefix = "~/projects"
remote_prefix = "~/dev"

[hosts.sandbox]
prefix = "/home/user/workspace"
binary = "/usr/local/bin/async"

[hosts.remote-server]
prefix = "/home/user/dev"
`
	if err := os.WriteFile(cfgFile, []byte(content), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	home, _ := os.UserHomeDir()
	wantLocal := filepath.Join(home, "projects")
	if cfg.LocalPrefix != wantLocal {
		t.Errorf("LocalPrefix = %q, want %q", cfg.LocalPrefix, wantLocal)
	}
	if cfg.RemotePrefix != "~/dev" {
		t.Errorf("RemotePrefix = %q, want ~/dev", cfg.RemotePrefix)
	}
	if got := cfg.HostPrefix("sandbox"); got != "/home/user/workspace" {
		t.Errorf("HostPrefix(sandbox) = %q, want /home/user/workspace", got)
	}
	if got := cfg.HostBinary("sandbox"); got != "/usr/local/bin/async" {
		t.Errorf("HostBinary(sandbox) = %q, want /usr/local/bin/async", got)
	}
	if got := cfg.HostPrefix("remote-server"); got != "/home/user/dev" {
		t.Errorf("HostPrefix(remote-server) = %q, want /home/user/dev", got)
	}
	if got := cfg.HostPrefix("unknown-host"); got != "~/dev" {
		t.Errorf("HostPrefix(unknown-host) = %q, want ~/dev", got)
	}
}

func TestLoadMissingConfigReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("unexpected error on missing file: %v", err)
	}
	if got := cfg.HostPrefix("any-host"); got != "~/workspace" {
		t.Errorf("default host prefix = %q, want ~/workspace", got)
	}
}
