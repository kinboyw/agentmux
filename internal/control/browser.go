package control

import (
	"os/exec"
	"runtime"
	"strings"
)

func OpenBrowserBestEffort(rawURL string) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	_ = cmd.Start()
}
