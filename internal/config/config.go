package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HostConfig holds per-host synchronization settings.
type HostConfig struct {
	Prefix    string
	CodexHome string
	AGYHome   string
	Binary    string
}

// Config represents the global async configuration.
type Config struct {
	LocalPrefix  string
	RemotePrefix string
	CodexHome    string
	AGYHome      string
	Hosts        map[string]HostConfig
}

// Default returns a Config initialized with standard defaults.
func Default() *Config {
	home, _ := os.UserHomeDir()
	localPrefix := filepath.Join(home, "projects")
	if st, err := os.Stat(localPrefix); err != nil || !st.IsDir() {
		alt := filepath.Join(home, "dev")
		if st2, err2 := os.Stat(alt); err2 == nil && st2.IsDir() {
			localPrefix = alt
		} else {
			localPrefix = filepath.Join(home, "workspace")
		}
	}

	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}

	agyHome := os.Getenv("AGY_HOME")
	if agyHome == "" {
		agyHome = filepath.Join(home, ".gemini", "antigravity-cli")
	}

	return &Config{
		LocalPrefix:  localPrefix,
		RemotePrefix: "~/workspace",
		CodexHome:    codexHome,
		AGYHome:      agyHome,
		Hosts:        make(map[string]HostConfig),
	}
}

// Load reads configuration from ~/.config/async/config.toml if it exists,
// falling back to Default() values.
func Load(customPath string) (*Config, error) {
	cfg := Default()
	if customPath == "" {
		if envPath := os.Getenv("ASYNC_CONFIG"); envPath != "" {
			customPath = envPath
		} else {
			home, err := os.UserHomeDir()
			if err == nil {
				customPath = filepath.Join(home, ".config", "async", "config.toml")
			}
		}
	}

	if customPath == "" {
		return cfg, nil
	}

	f, err := os.Open(customPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("opening config %s: %w", customPath, err)
	}
	defer f.Close()

	if err := parseTOML(f, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", customPath, err)
	}
	return cfg, nil
}

// HostPrefix returns the configured workspace base prefix for the given target host.
func (c *Config) HostPrefix(host string) string {
	if h, ok := c.Hosts[host]; ok && h.Prefix != "" {
		return h.Prefix
	}
	return c.RemotePrefix
}

// HostBinary returns the remote async binary path for the given target host.
func (c *Config) HostBinary(host string) string {
	if h, ok := c.Hosts[host]; ok && h.Binary != "" {
		return h.Binary
	}
	return "~/.local/bin/async"
}

// ExpandLocalPath expands a leading "~/" using the current user's home directory.
func ExpandLocalPath(p string) string {
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func parseTOML(f *os.File, cfg *Config) error {
	scanner := bufio.NewScanner(f)
	section := ""
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return fmt.Errorf("line %d: expected key = value", lineNum)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)

		switch {
		case section == "" || section == "defaults":
			switch key {
			case "local_prefix":
				cfg.LocalPrefix = ExpandLocalPath(val)
			case "remote_prefix":
				cfg.RemotePrefix = val
			case "codex_home":
				cfg.CodexHome = ExpandLocalPath(val)
			case "agy_home":
				cfg.AGYHome = ExpandLocalPath(val)
			}
		case strings.HasPrefix(section, "hosts."):
			hostName := strings.TrimPrefix(section, "hosts.")
			hostName = strings.Trim(hostName, `"'`)
			hc := cfg.Hosts[hostName]
			switch key {
			case "prefix":
				hc.Prefix = val
			case "codex_home":
				hc.CodexHome = val
			case "agy_home":
				hc.AGYHome = val
			case "binary":
				hc.Binary = val
			}
			cfg.Hosts[hostName] = hc
		}
	}
	return scanner.Err()
}
