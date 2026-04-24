package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the persisted state of an enrolled agent: the signaling endpoint
// it talks to plus the bearer token the server issued at enrollment.
type Config struct {
	ServerURL   string `json:"server_url"`   // e.g. https://connect.example.com
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"` // secret — this file should be 0600
	DeviceName  string `json:"device_name"`
}

// DefaultConfigPath picks a reasonable per-OS location:
//   Linux/BSD: $XDG_CONFIG_HOME/connect-agent/config.json, or ~/.config/...
//   macOS:     ~/Library/Application Support/connect-agent/config.json
//   Windows:   %AppData%\connect-agent\config.json
func DefaultConfigPath() (string, error) {
	if v := os.Getenv("CONNECT_AGENT_CONFIG"); v != "" {
		return v, nil
	}
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("APPDATA")
		if base == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(h, "AppData", "Roaming")
		}
	case "darwin":
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(h, "Library", "Application Support")
	default:
		base = os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(h, ".config")
		}
	}
	return filepath.Join(base, "connect-agent", "config.json"), nil
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

func SaveConfig(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Write atomically so a crash mid-save doesn't corrupt the file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var ErrNotEnrolled = errors.New("agent is not enrolled; run `agent enroll` first")
