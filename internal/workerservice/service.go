package workerservice

import (
	"context"
	"fmt"
	"html"
	"io"
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

type WorkerIdentity struct {
	ID string
}

func Start(ctx context.Context, binary string, identity WorkerIdentity) (Result, error) {
	binary, err := normalizeBinary(binary)
	if err != nil {
		return Result{}, err
	}
	binary, err = prepareWorkerBinary(binary)
	if err != nil {
		return Result{}, err
	}
	var skipped []string
	if runtime.GOOS == "linux" {
		if err := systemdUserCheck(ctx); err == nil {
			result, err := startSystemd(ctx, binary)
			if err == nil {
				return result, nil
			}
			skipped = append(skipped, serviceFallbackNote("systemd --user start", err))
		} else {
			skipped = append(skipped, serviceUnavailableNote("systemd --user", err))
		}
	}
	if runtime.GOOS == "darwin" {
		result, err := startLaunchd(ctx, binary)
		if err == nil {
			return result, nil
		}
		skipped = append(skipped, serviceUnavailableNote("launchd", err))
	}
	result, err := startFallback(binary, identity)
	if err != nil {
		return Result{}, err
	}
	if len(skipped) > 0 {
		result.Detail += "\n" + strings.Join(skipped, "\n")
	}
	return result, nil
}

func Restart(ctx context.Context, binary string, identity WorkerIdentity) (Result, error) {
	binary, err := normalizeBinary(binary)
	if err != nil {
		return Result{}, err
	}
	binary, err = prepareWorkerBinary(binary)
	if err != nil {
		return Result{}, err
	}
	var skipped []string
	if runtime.GOOS == "linux" {
		if err := systemdUserCheck(ctx); err != nil {
			skipped = append(skipped, serviceUnavailableNote("systemd --user", err))
		} else if systemdServiceExists() {
			if err := writeSystemdUnit(binary); err != nil {
				skipped = append(skipped, serviceFallbackNote("systemd --user unit update", err))
			} else if _, err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
				skipped = append(skipped, serviceFallbackNote("systemd --user daemon-reload", err))
			} else if out, err := run(ctx, "systemctl", "--user", "restart", ServiceName+".service"); err == nil {
				return Result{Backend: "systemd", Detail: strings.TrimSpace(out)}, nil
			} else {
				skipped = append(skipped, serviceFallbackNote("systemd --user restart", err))
			}
		} else {
			result, err := startSystemd(ctx, binary)
			if err == nil {
				return result, nil
			}
			skipped = append(skipped, serviceFallbackNote("systemd --user start", err))
		}
	}
	if runtime.GOOS == "darwin" {
		if launchdServiceExists() {
			_, _ = run(ctx, "launchctl", "unload", "-w", launchdPlistPath())
			if result, err := startLaunchd(ctx, binary); err == nil {
				return result, nil
			} else {
				skipped = append(skipped, serviceFallbackNote("launchd restart", err))
			}
		} else {
			result, err := startLaunchd(ctx, binary)
			if err == nil {
				return result, nil
			}
			skipped = append(skipped, serviceUnavailableNote("launchd", err))
		}
	}
	result, err := restartFallback(binary, identity)
	if err != nil {
		return Result{}, err
	}
	if len(skipped) > 0 {
		result.Detail += "\n" + strings.Join(skipped, "\n")
	}
	return result, nil
}

func Stop(ctx context.Context, identity WorkerIdentity) (Result, error) {
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
	return stopFallback(identity)
}

func Status(ctx context.Context, identity WorkerIdentity) (string, error) {
	var skipped []string
	if runtime.GOOS == "linux" && systemdServiceExists() && systemdUserAvailable(ctx) {
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
	out, err := fallbackStatus(identity)
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
	if runtime.GOOS == "linux" && systemdServiceExists() && systemdUserAvailable(ctx) {
		out, err := run(ctx, "journalctl", "--user", "-u", ServiceName+".service", "-n", strconv.Itoa(lines), "--no-pager")
		if err == nil && strings.TrimSpace(out) != "" && !strings.Contains(out, "-- No entries --") {
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

func FollowLogs(ctx context.Context, lines int, out io.Writer) error {
	if lines <= 0 {
		lines = 80
	}
	if runtime.GOOS == "linux" && systemdServiceExists() && systemdUserAvailable(ctx) {
		cmd := exec.CommandContext(ctx, "journalctl", "--user", "-u", ServiceName+".service", "-n", strconv.Itoa(lines), "-f")
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err == nil || ctx.Err() != nil {
			return ctx.Err()
		}
	}
	logPath, err := LogPath()
	if err != nil {
		return err
	}
	offset := int64(0)
	if data, err := os.ReadFile(logPath); err == nil {
		text := tailLines(string(data), lines)
		if text != "" {
			_, _ = io.WriteString(out, text)
			if !strings.HasSuffix(text, "\n") {
				_, _ = io.WriteString(out, "\n")
			}
		}
		offset = int64(len(data))
	} else if os.IsNotExist(err) {
		_, _ = fmt.Fprintf(out, "waiting for worker log: %s\n", logPath)
	} else {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			file, err := os.Open(logPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			info, err := file.Stat()
			if err != nil {
				_ = file.Close()
				return err
			}
			if info.Size() < offset {
				offset = 0
			}
			if info.Size() > offset {
				if _, err := file.Seek(offset, io.SeekStart); err == nil {
					n, _ := io.Copy(out, file)
					offset += n
				}
			}
			_ = file.Close()
		}
	}
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

func WorkerLockPath(workerID string) (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worker-"+SanitizeWorkerID(workerID)+".lock"), nil
}

func SanitizeWorkerID(workerID string) string {
	name := strings.TrimSpace(workerID)
	if name == "" {
		name = "default"
	}
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_").Replace(name)
}

func WriteLockOwner(lockFile *os.File, pid int) {
	if lockFile == nil || pid <= 0 {
		return
	}
	_ = lockFile.Truncate(0)
	_, _ = lockFile.Seek(0, 0)
	_, _ = fmt.Fprintf(lockFile, "%d\n", pid)
	_ = lockFile.Sync()
}

func ClearLockOwner(lockFile *os.File) {
	if lockFile == nil {
		return
	}
	_ = lockFile.Truncate(0)
	_, _ = lockFile.Seek(0, 0)
	_ = lockFile.Sync()
}

func LockOwnerPID(workerID string) (int, string, bool) {
	path, err := WorkerLockPath(workerID)
	if err != nil {
		return 0, "", false
	}
	if pid, ok := readPID(path); ok && workerProcessRunning(pid) {
		return pid, path, true
	}
	if pid, ok := procLockOwnerPID(path); ok && workerProcessRunning(pid) {
		return pid, path, true
	}
	return 0, path, false
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

func prepareWorkerBinary(binary string) (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(filepath.Base(binary), "agentmux-worker-bin.") && filepath.Dir(binary) == dir {
		return binary, nil
	}
	target := filepath.Join(dir, fmt.Sprintf("agentmux-worker-bin.%d.%d", time.Now().UnixNano(), os.Getpid()))
	in, err := os.Open(binary)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(target)
		return "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(target)
		return "", err
	}
	if err := os.Chmod(target, 0o700); err != nil {
		_ = os.Remove(target)
		return "", err
	}
	return target, nil
}

func workerRunArgs() []string {
	return []string{"worker", "run"}
}

func commandLine(binary string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, systemdQuote(binary))
	for _, arg := range args {
		parts = append(parts, systemdQuote(arg))
	}
	return strings.Join(parts, " ")
}

func startSystemd(ctx context.Context, binary string) (Result, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return Result{}, err
	}
	if err := systemdUserCheck(ctx); err != nil {
		return Result{}, err
	}
	if err := writeSystemdUnit(binary); err != nil {
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

func writeSystemdUnit(binary string) error {
	path, err := systemdServicePath()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	content := `[Unit]
Description=AgentMux Worker
After=network-online.target

[Service]
Type=simple
Environment=` + systemdQuote("HOME="+home) + `
Environment=` + systemdQuote("PATH="+workerServicePath()) + `
Environment=` + systemdQuote("AGENTMUX_WORKER_INSTALL_KIND=service") + `
Environment=` + systemdQuote("AGENTMUX_WORKER_SERVICE_BACKEND=systemd-user") + `
ExecStart=` + commandLine(binary, workerRunArgs()) + `
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
`
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	return nil
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
	envXML := launchdEnvXML(map[string]string{
		"PATH":                            workerServicePath(),
		"AGENTMUX_WORKER_INSTALL_KIND":    "service",
		"AGENTMUX_WORKER_SERVICE_BACKEND": "launchd",
		"AGENTMUX_TMUX":                   os.Getenv("AGENTMUX_TMUX"),
		"AGENTMUX_TMUX_PATH":              os.Getenv("AGENTMUX_TMUX_PATH"),
	})
	content := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>EnvironmentVariables</key>
  <dict>
` + envXML + `
  </dict>
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

func launchdEnvXML(values map[string]string) string {
	keys := []string{
		"PATH",
		"AGENTMUX_WORKER_INSTALL_KIND",
		"AGENTMUX_WORKER_SERVICE_BACKEND",
		"AGENTMUX_TMUX",
		"AGENTMUX_TMUX_PATH",
	}
	var b strings.Builder
	for _, key := range keys {
		value := strings.TrimSpace(values[key])
		if value == "" {
			continue
		}
		b.WriteString("    <key>")
		b.WriteString(html.EscapeString(key))
		b.WriteString("</key><string>")
		b.WriteString(html.EscapeString(value))
		b.WriteString("</string>\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func workerServicePath() string {
	return joinUniquePathEntries(append(strings.Split(os.Getenv("PATH"), ":"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/opt/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	)...)
}

func joinUniquePathEntries(entries ...string) string {
	seen := map[string]bool{}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		result = append(result, entry)
	}
	return strings.Join(result, ":")
}

func startFallback(binary string, identity WorkerIdentity) (Result, error) {
	pidPath, err := PIDPath()
	if err != nil {
		return Result{}, err
	}
	if pid, ok := readPID(pidPath); ok && workerProcessRunning(pid) {
		return Result{Backend: "process", Detail: fmt.Sprintf("already running pid=%d", pid)}, nil
	}
	if identity.ID != "" {
		if pid, path, ok := LockOwnerPID(identity.ID); ok && workerProcessRunning(pid) {
			return Result{Backend: "process", Detail: fmt.Sprintf("already running pid=%d lock=%s", pid, path)}, nil
		}
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
	args := workerRunArgs()
	cmd := exec.Command(binary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), "AGENTMUX_WORKER_INSTALL_KIND=service", "AGENTMUX_WORKER_SERVICE_BACKEND=process")
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

func restartFallback(binary string, identity WorkerIdentity) (Result, error) {
	pidPath, err := PIDPath()
	if err != nil {
		return Result{}, err
	}
	stopped := make([]string, 0, 2)
	stoppedPIDs := map[int]bool{}
	if pid, ok := readPID(pidPath); ok {
		if workerProcessRunning(pid) {
			result, err := stopFallbackPID(pidPath, pid)
			if err != nil {
				return Result{}, err
			}
			stopped = append(stopped, result.Detail)
			stoppedPIDs[pid] = true
		}
		_ = os.Remove(pidPath)
	}
	if identity.ID != "" {
		if pid, _, ok := LockOwnerPID(identity.ID); ok && workerProcessRunning(pid) && !stoppedPIDs[pid] {
			result, err := stopFallbackPID(pidPath, pid)
			if err != nil {
				return Result{}, err
			}
			stopped = append(stopped, result.Detail)
		}
	}
	result, err := startFallback(binary, identity)
	if err != nil {
		return Result{}, err
	}
	if len(stopped) > 0 {
		result.Detail = strings.Join(stopped, "\n") + "\n" + result.Detail
	}
	return result, nil
}

func stopFallback(identity WorkerIdentity) (Result, error) {
	pidPath, err := PIDPath()
	if err != nil {
		return Result{}, err
	}
	pid, ok := readPID(pidPath)
	if !ok {
		if identity.ID != "" {
			if lockPID, _, lockOK := LockOwnerPID(identity.ID); lockOK && workerProcessRunning(lockPID) {
				return stopFallbackPID(pidPath, lockPID)
			}
		}
		return Result{Backend: "process", Detail: "not running"}, nil
	}
	if !workerProcessRunning(pid) {
		_ = os.Remove(pidPath)
		if identity.ID != "" {
			if lockPID, _, lockOK := LockOwnerPID(identity.ID); lockOK && workerProcessRunning(lockPID) {
				return stopFallbackPID(pidPath, lockPID)
			}
		}
		return Result{Backend: "process", Detail: "not running"}, nil
	}
	return stopFallbackPID(pidPath, pid)
}

func stopFallbackPID(pidPath string, pid int) (Result, error) {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return Result{}, err
	}
	detail := fmt.Sprintf("stopped pid=%d", pid)
	if !waitForExit(pid, 2*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = waitForExit(pid, time.Second)
		detail = fmt.Sprintf("killed pid=%d", pid)
	}
	_ = os.Remove(pidPath)
	return Result{Backend: "process", Detail: detail}, nil
}

func fallbackStatus(identity WorkerIdentity) (string, error) {
	pidPath, err := PIDReadPath()
	if err != nil {
		return "", err
	}
	pid, ok := readPID(pidPath)
	if !ok || !workerProcessRunning(pid) {
		if identity.ID != "" {
			if lockPID, lockPath, lockOK := LockOwnerPID(identity.ID); lockOK && workerProcessRunning(lockPID) {
				logPath, _ := LogPath()
				return fmt.Sprintf("agentmux worker fallback process is running\npid=%d\nlock=%s\nlog=%s\n", lockPID, lockPath, logPath), nil
			}
		}
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

func tailLines(value string, lines int) string {
	if lines <= 0 {
		lines = 80
	}
	parts := strings.Split(value, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
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

func systemdUserAvailable(ctx context.Context) bool {
	return systemdUserCheck(ctx) == nil
}

func systemdUserCheck(ctx context.Context) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return err
	}
	_, err := run(ctx, "systemctl", "--user", "show", "--property=Version", "--value")
	return err
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

func serviceUnavailableNote(name string, err error) string {
	reason := conciseServiceReason(err)
	if reason == "" {
		return fmt.Sprintf("note: %s unavailable; using fallback process", name)
	}
	return fmt.Sprintf("note: %s unavailable; using fallback process (%s)", name, reason)
}

func serviceFallbackNote(action string, err error) string {
	reason := conciseServiceReason(err)
	if reason == "" {
		return fmt.Sprintf("note: %s did not complete; using fallback process", action)
	}
	return fmt.Sprintf("note: %s did not complete; using fallback process (%s)", action, reason)
}

func conciseServiceReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	switch {
	case strings.Contains(msg, "Failed to connect to bus"):
		return "systemd user bus is not available"
	case strings.Contains(msg, "executable file not found"):
		return "command not found"
	case msg == "":
		return ""
	default:
		return msg
	}
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

func procLockOwnerPID(path string) (int, bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		return 0, false
	}
	device := fmt.Sprintf("%02x:%02x", major(uint64(stat.Dev)), minor(uint64(stat.Dev)))
	inode := strconv.FormatUint(stat.Ino, 10)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if fields[3] != "WRITE" {
			continue
		}
		if fields[5] == "" {
			continue
		}
		lockParts := strings.Split(fields[5], ":")
		if len(lockParts) < 3 {
			continue
		}
		if lockParts[0]+":"+lockParts[1] != device || lockParts[2] != inode {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}

func major(dev uint64) uint64 {
	return (dev >> 8) & 0xfff
}

func minor(dev uint64) uint64 {
	return (dev & 0xff) | ((dev >> 12) & 0xfff00)
}

func processRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func workerProcessRunning(pid int) bool {
	if !processRunning(pid) {
		return false
	}
	match, known := processMatchesWorker(pid)
	if known {
		return match
	}
	return true
}

func processMatchesWorker(pid int) (bool, bool) {
	if runtime.GOOS != "linux" {
		return false, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false, false
	}
	fields := strings.Split(string(data), "\x00")
	args := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			args = append(args, field)
		}
	}
	if len(args) == 0 {
		return false, true
	}
	if !strings.HasPrefix(filepath.Base(args[0]), "agentmux") {
		return false, true
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "worker" && args[i+1] == "run" {
			return true, true
		}
	}
	return false, true
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
