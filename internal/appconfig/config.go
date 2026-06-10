package appconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	WorkerBackend string `json:"worker_backend,omitempty"`
	WorkerHubURL  string `json:"worker_hub_url,omitempty"`
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
	cfg.WorkerBackend = strings.TrimSpace(cfg.WorkerBackend)
	cfg.WorkerHubURL = strings.TrimSpace(cfg.WorkerHubURL)
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
	cfg.WorkerBackend = strings.TrimSpace(cfg.WorkerBackend)
	cfg.WorkerHubURL = strings.TrimSpace(cfg.WorkerHubURL)
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

func SaveWorkerBackend(backend string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.WorkerBackend = strings.TrimSpace(backend)
	return Save(cfg)
}
