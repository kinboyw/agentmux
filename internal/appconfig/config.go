package appconfig

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	ControlHubURL      string `json:"control_hub_url,omitempty"`
	WorkerBackend      string `json:"worker_backend,omitempty"`
	WorkerTerminalMode string `json:"worker_terminal_mode,omitempty"`
	WorkerStateCols    int    `json:"worker_state_cols,omitempty"`
	WorkerStateRows    int    `json:"worker_state_rows,omitempty"`
	WorkerHubURL       string `json:"worker_hub_url,omitempty"`
	WorkerInstanceID   string `json:"worker_instance_id,omitempty"`
	// WorkerToken is kept only for reading old config.json files. New writes
	// store credentials in credentials.json through credentialcache.
	WorkerToken string `json:"worker_token,omitempty"`
	WorkerID    string `json:"worker_id,omitempty"`
	WorkerName  string `json:"worker_name,omitempty"`
}

func Path() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agentmux", "config.json"), nil
}

func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg.ControlHubURL = strings.TrimSpace(cfg.ControlHubURL)
	cfg.WorkerBackend = strings.TrimSpace(cfg.WorkerBackend)
	cfg.WorkerTerminalMode = strings.TrimSpace(cfg.WorkerTerminalMode)
	cfg.WorkerHubURL = strings.TrimSpace(cfg.WorkerHubURL)
	cfg.WorkerInstanceID = strings.TrimSpace(cfg.WorkerInstanceID)
	cfg.WorkerToken = strings.TrimSpace(cfg.WorkerToken)
	cfg.WorkerID = strings.TrimSpace(cfg.WorkerID)
	cfg.WorkerName = strings.TrimSpace(cfg.WorkerName)
	return cfg, nil
}

func Save(cfg Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cfg.ControlHubURL = strings.TrimSpace(cfg.ControlHubURL)
	cfg.WorkerBackend = strings.TrimSpace(cfg.WorkerBackend)
	cfg.WorkerTerminalMode = strings.TrimSpace(cfg.WorkerTerminalMode)
	cfg.WorkerHubURL = strings.TrimSpace(cfg.WorkerHubURL)
	cfg.WorkerInstanceID = strings.TrimSpace(cfg.WorkerInstanceID)
	cfg.WorkerToken = ""
	cfg.WorkerID = strings.TrimSpace(cfg.WorkerID)
	cfg.WorkerName = strings.TrimSpace(cfg.WorkerName)
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func SaveControlHubURL(hubURL string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.ControlHubURL = strings.TrimSpace(hubURL)
	return Save(cfg)
}

func EnsureWorkerInstanceID() (string, error) {
	cfg, err := Load()
	if err != nil {
		return "", err
	}
	if cfg.WorkerInstanceID != "" {
		return cfg.WorkerInstanceID, nil
	}
	cfg.WorkerInstanceID = "wins_" + randomHex(16)
	if err := Save(cfg); err != nil {
		return "", err
	}
	return cfg.WorkerInstanceID, nil
}

func SaveWorkerAuth(hubURL, _ string, id, name string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.WorkerHubURL = strings.TrimSpace(hubURL)
	cfg.WorkerToken = ""
	cfg.WorkerID = strings.TrimSpace(id)
	cfg.WorkerName = strings.TrimSpace(name)
	return Save(cfg)
}

func ClearWorkerAuth() error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.WorkerHubURL = ""
	cfg.WorkerToken = ""
	cfg.WorkerID = ""
	cfg.WorkerName = ""
	return Save(cfg)
}

func SaveWorkerBackend(backend string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.WorkerBackend = strings.TrimSpace(backend)
	return Save(cfg)
}

func SaveWorkerTerminalState(mode string, cols int, rows int) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(mode) != "" {
		cfg.WorkerTerminalMode = strings.TrimSpace(mode)
	}
	if cols > 0 {
		cfg.WorkerStateCols = cols
	}
	if rows > 0 {
		cfg.WorkerStateRows = rows
	}
	return Save(cfg)
}

func randomHex(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(buf)
}
