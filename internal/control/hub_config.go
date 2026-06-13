package control

import (
	"os"
	"strings"

	"private/agentmux/internal/appconfig"
	"private/agentmux/internal/credentialcache"
)

const SystemDefaultHubURL = "https://agentmux.kinboy.wang"

func DefaultHubURL() string {
	if value := firstNonEmptyEnv("AGENTMUX_CONTROL_HUB", "AGENTMUX_HUB"); value != "" {
		return credentialcache.NormalizeHubURL(value)
	}
	if cfg, err := appconfig.Load(); err == nil && strings.TrimSpace(cfg.ControlHubURL) != "" {
		return credentialcache.NormalizeHubURL(cfg.ControlHubURL)
	}
	if entry, ok := credentialcache.LoadLatest("control", ""); ok && strings.TrimSpace(entry.HubURL) != "" {
		return credentialcache.NormalizeHubURL(entry.HubURL)
	}
	if cfg, err := appconfig.Load(); err == nil && strings.TrimSpace(cfg.WorkerHubURL) != "" {
		return credentialcache.NormalizeHubURL(cfg.WorkerHubURL)
	}
	if entry, ok := credentialcache.LoadLatest("worker", ""); ok && strings.TrimSpace(entry.HubURL) != "" {
		return credentialcache.NormalizeHubURL(entry.HubURL)
	}
	return SystemDefaultHubURL
}

func NormalizeHubURL(hubURL string) string {
	return credentialcache.NormalizeHubURL(hubURL)
}

func RememberHubURL(hubURL string) error {
	hubURL = credentialcache.NormalizeHubURL(hubURL)
	if hubURL == "" {
		return nil
	}
	return appconfig.SaveControlHubURL(hubURL)
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}
