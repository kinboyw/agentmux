package workerservice

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	ServiceName  = "agentmux-worker"
	launchdLabel = "com.agentmux.worker"
)

type Result struct {
	Backend string
	Detail  string
}

func Start(ctx context.Context, binary string) (Result, error) {
	binary, err := normalizeBinary(binary)
	if err != nil {
		return Result{}, err
	}
	var skipped []string
	if runtime.GOOS == "linux" {
		result, err := startSystemd(ctx, binary)
		if err == nil {
			return result, nil
		}
		skipped = append(skipped, "systemd --user unavailable: "+err.Error())
	}
	if runtime.GOOS == "darwin" {
		result, err := startLaunchd(ctx, binary)
		if err == nil {
			return result, nil
		}
		skipped = append(skipped, "launchd unavailable: "+err.Error())
	}
	result, err := startFallback(binary)
	if err != nil {
		return Result{}, err
	}
	if len(skipped) > 0 {
		result.Detail += "\n" + strings.Join(skipped, "\n")
	}
	return result, nil
}

func Restart(ctx context.Context, binary string) (Result, error) {
	binary, err := normalizeBinary(binary)
	if err != nil {
		return Result{}, err
	}
	var skipped []string
	if runtime.GOOS == "linux" {
		if systemdServiceExists() {
			if out, err := run(ctx, "systemctl", "--user", "restart", ServiceName+".service"); err == nil {
				return Result{Backend: "systemd", Detail: strings.TrimSpace(out)}, nil
			} else {
				skipped = append(skipped, "systemd --user restart unavailable: "+err.Error())
			}
		} else {
			result, err := startSystemd(ctx, binary)
			if err == nil {
				return result, nil
			}
			skipped = append(skipped, "systemd --user unavailable: "+err.Error())
		}
	}
	if runtime.GOOS == "darwin" {
		if launchdServiceExists() {
			_, _ = run(ctx, "launchctl", "unload", "-w", launchdPlistPath())
			if result, err := startLaunchd(ctx, binary); err == nil {
				return result, nil
			} else {
				skipped = append(skipped, "launchd restart unavailable: "+err.Error())
			}
		} else {
			result, err := startLaunchd(ctx, binary)
			if err == nil {
				return result, nil
			}
			skipped = append(skipped, "launchd unavailable: "+err.Error())
		}
	}
	result, err := restartFallback(binary)
	if err != nil {
		return Result{}, err
	}
	if len(skipped) > 0 {
		result.Detail += "\n" + strings.Join(skipped, "\n")
	}
	return result, nil
}

func Stop(ctx context.Context) (Result, error) {
	if runtime.GOOS == "linux" && systemdServiceExists() {
		if out, err := run(ctx, "systemctl", "--user", "stop", ServiceName+".service"); err == nil {
			return Result{Backend: "systemd", Detail: strings.TrimSpace(out)}, nil
		}
	}
	if runtime.GOOS == "darwin" && launchdServiceExists() {
		if out, err := run(ctx, "launchctl", "unload", "-w", launchdPlistPath()); err == nil {
			return Result{Backend: "launchd", Detail: strings.TrimSpace(out)}, nil
		}
	}
	return stopFallback()
}

func Status(ctx context.Context) (string, error) {
	var skipped []string
	if runtime.GOOS == "linux" && systemdServiceExists() {
		out, err := run(ctx, "systemctl", "--user", "status", ServiceName+".service", "--no-pager", "-l")
		if err == nil && out != "" {
			return out, nil
		}
		if err != nil {
			skipped = append(skipped, "systemd --user unavailable: "+err.Error())
		}
	}
	if runtime.GOOS == "darwin" && launchdServiceExists() {
		out, err := run(ctx, "launchctl", "list", launchdLabel)
		if err == nil && out != "" {
			return out, nil
		}
		if err != nil {
			skipped = append(skipped, "launchd unavailable: "+err.Error())
		}
	}
	out, err := fallbackStatus()
	if len(skipped) > 0 {
		out += strings.Join(skipped, "\n") + "\n"
	}
	return out, err
}

func Logs(ctx context.Context, lines int) (string, error) {
	if lines <= 0 {
		lines = 80
	}
	var skipped []string
	if runtime.GOOS == "linux" && systemdServiceExists() {
		out, err := run(ctx, "journalctl", "--user", "-u", ServiceName+".service", "-n", strconv.Itoa(lines), "--no-pager")
		if err == nil && out != "" {
			return out, nil
		}
		if err != nil {
			skipped = append(skipped, "systemd --user logs unavailable: "+err.Error())
		}
	}
	out, err := fallbackLogs(lines)
	if len(skipped) > 0 {
		out += strings.Join(skipped, "\n") + "\n"
	}
	return out, err
}

func StateDir() (string, error) {
	return stateDir(true)
}

func stateDir(create bool) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", homeErr
		}
		base = filepath.Join(home, ".local", "state")
	}
	path := filepath.Join(base, "agentmux")
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
	}
	return path, nil
}

func StateReadDir() (string, error) {
	return stateDir(false)
}

func LogPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worker.log"), nil
}

func LogReadPath() (string, error) {
	dir, err := StateReadDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worker.log"), nil
}

func PIDPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worker.pid"), nil
}

func PIDReadPath() (string, error) {
	dir, err := StateReadDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worker.pid"), nil
}

func normalizeBinary(binary string) (string, error) {
	if binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		binary = exe
	}
	if !filepath.IsAbs(binary) {
		abs, err := filepath.Abs(binary)
		if err == nil {
			binary = abs
		}
	}
	return binary, nil
}

func startSystemd(ctx context.Context, binary string) (Result, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return Result{}, err
	}
	path, err := systemdServicePath()
	if err != nil {
		return Result{}, err
	}
	content := `[Unit]
Description=AgentMux Worker
After=network-online.target

[Service]
Type=simple
ExecStart=` + systemdQuote(binary) + ` worker run
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return Result{}, err
	}
	if _, err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return Result{}, err
	}
	if out, err := run(ctx, "systemctl", "--user", "enable", "--now", ServiceName+".service"); err != nil {
		return Result{}, err
	} else {
		return Result{Backend: "systemd", Detail: strings.TrimSpace(out)}, nil
	}
}

func startLaunchd(ctx context.Context, binary string) (Result, error) {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return Result{}, err
	}
	plist := launchdPlistPath()
	logPath, err := LogPath()
	if err != nil {
		return Result{}, err
	}
	content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + html.EscapeString(binary) + `</string>
    <string>worker</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + html.EscapeString(logPath) + `</string>
  <key>StandardErrorPath</key><string>` + html.EscapeString(logPath) + `</string>
</dict>
</plist>
`
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(plist, []byte(content), 0o600); err != nil {
		return Result{}, err
	}
	_, _ = run(ctx, "launchctl", "unload", "-w", plist)
	if out, err := run(ctx, "launchctl", "load", "-w", plist); err != nil {
		return Result{}, err
	} else {
		return Result{Backend: "launchd", Detail: strings.TrimSpace(out)}, nil
	}
}

func startFallback(binary string) (Result, error) {
	pidPath, err := PIDPath()
	if err != nil {
		return Result{}, err
	}
	if pid, ok := readPID(pidPath); ok && processRunning(pid) {
		return Result{Backend: "process", Detail: fmt.Sprintf("already running pid=%d", pid)}, nil
	}
	logPath, err := LogPath()
	if err != nil {
		return Result{}, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer logFile.Close()
	cmd := exec.Command(binary, "worker", "run")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		return Result{}, err
	}
	time.Sleep(150 * time.Millisecond)
	if !processRunning(cmd.Process.Pid) {
		return Result{}, fmt.Errorf("worker process exited after start; check %s", logPath)
	}
	return Result{Backend: "process", Detail: fmt.Sprintf("pid=%d log=%s", cmd.Process.Pid, logPath)}, nil
}

func restartFallback(binary string) (Result, error) {
	pidPath, err := PIDPath()
	if err != nil {
		return Result{}, err
	}
	var stopped string
	if pid, ok := readPID(pidPath); ok {
		if processRunning(pid) {
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				return Result{}, err
			}
			if waitForExit(pid, 2*time.Second) {
				stopped = fmt.Sprintf("stopped pid=%d", pid)
			} else {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				_ = waitForExit(pid, time.Second)
				stopped = fmt.Sprintf("killed pid=%d", pid)
			}
		}
		_ = os.Remove(pidPath)
	}
	result, err := startFallback(binary)
	if err != nil {
		return Result{}, err
	}
	if stopped != "" {
		result.Detail = stopped + "\n" + result.Detail
	}
	return result, nil
}

func stopFallback() (Result, error) {
	pidPath, err := PIDPath()
	if err != nil {
		return Result{}, err
	}
	pid, ok := readPID(pidPath)
	if !ok {
		return Result{Backend: "process", Detail: "not running"}, nil
	}
	if !processRunning(pid) {
		_ = os.Remove(pidPath)
		return Result{Backend: "process", Detail: "not running"}, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return Result{}, err
	}
	_ = os.Remove(pidPath)
	return Result{Backend: "process", Detail: fmt.Sprintf("stopped pid=%d", pid)}, nil
}

func fallbackStatus() (string, error) {
	pidPath, err := PIDReadPath()
	if err != nil {
		return "", err
	}
	pid, ok := readPID(pidPath)
	if !ok || !processRunning(pid) {
		return "agentmux worker fallback process is not running\n", nil
	}
	logPath, _ := LogPath()
	return fmt.Sprintf("agentmux worker fallback process is running\npid=%d\nlog=%s\n", pid, logPath), nil
}

func fallbackLogs(lines int) (string, error) {
	logPath, err := LogReadPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "no worker logs found\n", nil
		}
		return "", err
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n"), nil
}

func systemdServicePath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "systemd", "user", ServiceName+".service"), nil
}

func systemdServiceExists() bool {
	path, err := systemdServicePath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func launchdServiceExists() bool {
	_, err := os.Stat(launchdPlistPath())
	return err == nil
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func processRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processRunning(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !processRunning(pid)
}
