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
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func SaveWorkerBackend(backend string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	cfg.WorkerBackend = strings.TrimSpace(backend)
	return Save(cfg)
}
