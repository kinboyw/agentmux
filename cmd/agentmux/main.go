package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"private/agentmux/internal/appconfig"
	"private/agentmux/internal/control"
	"private/agentmux/internal/credentialcache"
	"private/agentmux/internal/hub"
	"private/agentmux/internal/protocol"
	"private/agentmux/internal/ptybackend"
	"private/agentmux/internal/sessionbackend"
	"private/agentmux/internal/term"
	"private/agentmux/internal/tmux"
	"private/agentmux/internal/worker"
	"private/agentmux/internal/workerservice"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if isTUIInvocation() {
		runTUI(ctx, os.Args[1:])
		return
	}
	if len(os.Args) < 2 {
		runDefault(ctx)
		return
	}

	switch os.Args[1] {
	case "hub":
		runHub(ctx, os.Args[2:])
	case "worker":
		runWorker(ctx, os.Args[2:])
	case "control":
		runControl(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func isTUIInvocation() bool {
	return filepathBase(os.Args[0]) == "agentmux-tui"
}

func runDefault(ctx context.Context) {
	entry, ok := credentialcache.LoadLatest("", "")
	if !ok {
		usage()
		os.Exit(2)
	}
	switch entry.Role {
	case "worker":
		runWorkerWithAuth(ctx, entry.HubURL, entry.Credential, entry.DeviceID, entry.DeviceName, time.Second, "cache", "")
	case "control":
		defer term.ResetModes(os.Stdout)
		client := control.New(entry.HubURL, entry.Credential).WithCacheEntry(entry)
		auth := control.AppAuthResult{
			Client: client, CredentialID: entry.CredentialID,
			TenantID: entry.TenantID, DeviceID: entry.DeviceID, Role: entry.Role,
			ExpiresAt: entry.ExpiresAt, RefreshToken: entry.RefreshToken,
			RefreshExpiresAt: entry.RefreshExpiresAt, Source: "cache",
		}
		app := control.NewApp(auth.Client, auth, os.Stdin, os.Stdout)
		if err := app.Run(ctx); err != nil && err != context.Canceled {
			fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func runTUI(ctx context.Context, args []string) {
	defer term.ResetModes(os.Stdout)
	if len(args) > 0 && args[0] == "join" {
		fs := flag.NewFlagSet("join", flag.ExitOnError)
		common := addControlCommon(fs)
		deviceID := fs.String("device-id", "", "stable control device id")
		deviceName := fs.String("device-name", hostname(), "control device display name")
		_ = fs.Parse(args[1:])
		if common.hub == "" {
			common.hub = "http://127.0.0.1:8080"
		}
		if common.join == "" {
			fatal(fmt.Errorf("--join is required"))
		}
		if _, err := control.ResolveAppAuth(ctx, control.AppAuthOptions{
			HubURL: common.hub, Token: common.token, Join: common.join,
			DeviceID: *deviceID, DeviceName: *deviceName,
		}); err != nil {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, "control credential saved")
		return
	}
	auth, err := resolveControlAppAuthWithLogin(ctx, args, false)
	var app *control.App
	if err != nil {
		app = control.NewUnauthApp(os.Stdin, os.Stdout, err)
		if hubURL := controlHubArg(args); hubURL != "" {
			app.Client = control.New(hubURL, "")
		}
	} else {
		app = control.NewApp(auth.Client, auth, os.Stdin, os.Stdout)
	}
	if err := app.Run(ctx); err != nil && err != context.Canceled {
		fatal(err)
	}
}

func runHub(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	token := fs.String("token", os.Getenv("AGENTMUX_TOKEN"), "shared auth token")
	data := fs.String("data", os.Getenv("AGENTMUX_DATA"), "SQLite database path for persistent hub state")
	publicURL := fs.String("public-url", os.Getenv("AGENTMUX_PUBLIC_URL"), "external hub URL used for generated HTTPS/WSS commands")
	releaseRepo := fs.String("release-repo", getenv("AGENTMUX_RELEASE_REPO", "kinboyw/agentmux"), "GitHub owner/repo used by generated install.sh")
	_ = fs.Parse(args)
	var authStore hub.AuthStore
	if *data != "" {
		store, err := hub.OpenSQLiteAuthStore(*data)
		if err != nil {
			fatal(err)
		}
		defer func() {
			if err := store.Close(); err != nil {
				slog.Default().Warn("close sqlite store failed", "error", err)
			}
		}()
		authStore = store
		slog.Default().Info("hub persistence enabled", "data", *data)
	}
	server, err := hub.NewWithOptions(hub.ServerOptions{Addr: *addr, Token: *token, PublicURL: *publicURL, ReleaseRepo: *releaseRepo, Logger: slog.Default(), AuthStore: authStore})
	if err != nil {
		fatal(err)
	}
	if err := server.ListenAndServe(ctx); err != nil {
		fatal(err)
	}
}

func runWorker(ctx context.Context, args []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "join":
			runWorkerJoin(ctx, args[1:])
			return
		case "run":
			runWorkerForeground(ctx, args[1:])
			return
		case "start":
			runWorkerStart(ctx, args[1:]...)
			return
		case "stop":
			runWorkerStop(ctx)
			return
		case "status":
			runWorkerStatus(ctx)
			return
		case "logs":
			runWorkerLogs(ctx, args[1:])
			return
		case "config":
			runWorkerConfig(args[1:])
			return
		default:
			workerUsage()
			os.Exit(2)
		}
	}
	if hasJoinArg(args) {
		runWorkerJoin(ctx, args)
		return
	}
	runWorkerForeground(ctx, args)
}

func runWorkerForeground(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	hubURL := fs.String("hub", "", "hub URL")
	token := fs.String("token", os.Getenv("AGENTMUX_TOKEN"), "shared auth token")
	join := fs.String("join", "", "short-lived signal token")
	id := fs.String("id", "", "stable worker id")
	name := fs.String("name", hostname(), "worker display name")
	interval := fs.Duration("interval", time.Second, "terminal capture interval")
	backend := fs.String("backend", "", "session backend: auto, tmux, or pty")
	_ = fs.Parse(args)
	cfg, _ := appconfig.Load()
	if *hubURL == "" && *token == "" && *join == "" {
		*hubURL = cfg.WorkerHubURL
		if *id == "" {
			*id = cfg.WorkerID
		}
		if cfg.WorkerName != "" && (*name == "" || *name == hostname()) {
			*name = cfg.WorkerName
		}
		if entry, ok := workerCredentialFromCache(*hubURL, *id); ok {
			*hubURL = entry.HubURL
			*token = entry.Credential
			if *id == "" {
				*id = entry.DeviceID
			}
			if entry.DeviceName != "" && (*name == "" || *name == hostname()) {
				*name = entry.DeviceName
			}
		} else {
			*token = cfg.WorkerToken
			if cfg.WorkerToken != "" {
				if err := migrateLegacyWorkerCredential(cfg); err != nil {
					slog.Default().Warn("migrate legacy worker credential failed", "error", err)
				} else if err := appconfig.SaveWorkerAuth(cfg.WorkerHubURL, "", cfg.WorkerID, cfg.WorkerName); err != nil {
					slog.Default().Warn("clear legacy worker token failed", "error", err)
				}
			}
		}
	}
	if *hubURL == "" && (*token != "" || *join != "") {
		*hubURL = "ws://127.0.0.1:8080"
	}
	workerID := *id
	if workerID == "" && (*token != "" || *join != "") {
		workerID = *name
	}
	auth, err := worker.ResolveAuth(ctx, worker.AuthOptions{
		HubURL: *hubURL, Token: *token, Join: *join,
		DeviceID: workerID, DeviceName: *name,
	})
	if err != nil {
		fatal(err)
	}
	runWorkerWithAuth(ctx, auth.HubURL, auth.Token, workerIDFromAuth(auth, workerID), workerNameFromAuth(auth, *name), *interval, auth.Source, *backend)
}

func runWorkerJoin(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	hubURL := fs.String("hub", "", "hub URL")
	join := fs.String("join", "", "short-lived signal token")
	id := fs.String("id", "", "stable worker id")
	name := fs.String("name", hostname(), "worker display name")
	start := fs.Bool("start", true, "start background worker service after saving credential")
	backend := fs.String("backend", "", "session backend: auto, tmux, or pty")
	_ = fs.Parse(args)
	if *hubURL == "" {
		*hubURL = "ws://127.0.0.1:8080"
	}
	if *join == "" {
		fatal(fmt.Errorf("--join is required"))
	}
	workerID := *id
	if workerID == "" {
		workerID = *name
	}
	auth, err := worker.ResolveAuth(ctx, worker.AuthOptions{
		HubURL: *hubURL, Join: *join, DeviceID: workerID, DeviceName: *name,
	})
	if err != nil {
		fatal(err)
	}
	if err := appconfig.SaveWorkerAuth(auth.HubURL, "", workerIDFromAuth(auth, workerID), workerNameFromAuth(auth, *name)); err != nil {
		fatal(err)
	}
	slog.Default().Info("worker signal exchanged", "device_id", auth.DeviceID)
	if flagProvided(args, "--backend") {
		saveWorkerBackendConfig(*backend)
	}
	if !*start {
		return
	}
	if _, err := selectWorkerBackend(*backend); err != nil {
		fatal(err)
	}
	result, err := workerservice.Restart(ctx, executablePath(), workerservice.WorkerIdentity{ID: workerIDFromAuth(auth, workerID)})
	if err != nil {
		fatal(err)
	}
	slog.Default().Info("worker service restarted", "backend", result.Backend, "detail", result.Detail)
}

func runWorkerStart(ctx context.Context, args ...string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	backend := fs.String("backend", "", "session backend: auto, tmux, or pty")
	_ = fs.Parse(args)
	if flagProvided(args, "--backend") {
		saveWorkerBackendConfig(*backend)
	}
	if _, err := selectWorkerBackend(*backend); err != nil {
		fatal(err)
	}
	cfg, _ := appconfig.Load()
	if _, ok := workerCredentialFromCache(cfg.WorkerHubURL, cfg.WorkerID); !ok && (cfg.WorkerHubURL == "" || cfg.WorkerToken == "") {
		fatal(fmt.Errorf("no worker credential available; run agentmux worker join first"))
	}
	if cfg.WorkerToken != "" {
		if err := migrateLegacyWorkerCredential(cfg); err != nil {
			fatal(err)
		}
		if err := appconfig.SaveWorkerAuth(cfg.WorkerHubURL, "", cfg.WorkerID, cfg.WorkerName); err != nil {
			fatal(err)
		}
	}
	result, err := workerservice.Start(ctx, executablePath(), workerServiceIdentity())
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "worker service started (%s)\n%s\n", result.Backend, result.Detail)
}

func runWorkerStop(ctx context.Context) {
	result, err := workerservice.Stop(ctx, workerServiceIdentity())
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "worker service stopped (%s)\n%s\n", result.Backend, result.Detail)
}

func runWorkerStatus(ctx context.Context) {
	out, err := workerservice.Status(ctx, workerServiceIdentity())
	if err != nil {
		fatal(err)
	}
	fmt.Fprint(os.Stdout, out)
}

func runWorkerLogs(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	lines := fs.Int("n", 80, "number of log lines")
	_ = fs.Parse(args)
	out, err := workerservice.Logs(ctx, *lines)
	if err != nil {
		fatal(err)
	}
	fmt.Fprint(os.Stdout, out)
}

func runWorkerConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	backend := fs.String("backend", "", "session backend: auto, tmux, or pty")
	_ = fs.Parse(args)
	if flagProvided(args, "--backend") {
		saveWorkerBackendConfig(*backend)
	}
	cfg, err := appconfig.Load()
	if err != nil {
		fatal(err)
	}
	path, _ := appconfig.Path()
	credentialsPath, _ := credentialcache.Path()
	resolved, source := resolveWorkerBackendPreference("")
	fmt.Fprintf(os.Stdout, "config=%s\ncredentials=%s\nworker_backend=%s\nworker_hub_url=%s\nworker_id=%s\nworker_name=%s\nresolved_backend=%s\nsource=%s\n", path, credentialsPath, cfg.WorkerBackend, cfg.WorkerHubURL, cfg.WorkerID, cfg.WorkerName, resolved, source)
}

func runWorkerWithAuth(ctx context.Context, hubURL, token, workerID, name string, interval time.Duration, source string, backendPreference string) {
	lockFile, unlock, err := acquireWorkerLock(workerID)
	if err != nil {
		fatal(err)
	}
	defer unlock()
	backend, err := selectWorkerBackend(backendPreference)
	if err != nil {
		fatal(err)
	}
	switch source {
	case "join":
		slog.Default().Info("worker signal exchanged", "device_id", workerID)
	case "cache":
		slog.Default().Info("worker credential loaded", "device_id", workerID)
	}
	slog.Default().Info("worker runtime lock acquired", "lock", lockFile)
	w := worker.New(hubURL, token, workerID, name, backend, slog.Default())
	w.Interval = interval
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		fatal(err)
	}
}

func selectWorkerBackend(preference string) (sessionbackend.Backend, error) {
	value, source := resolveWorkerBackendPreference(preference)
	switch value {
	case "", "auto":
		if err := tmux.CheckAvailable(); err == nil {
			return tmux.New(nil), nil
		} else {
			warnPtyFallback(err, source)
			return ptybackend.New(), nil
		}
	case "tmux":
		if err := tmux.CheckAvailable(); err != nil {
			return nil, err
		}
		return tmux.New(nil), nil
	case "pty", "builtin", "builtin-pty":
		return ptybackend.New(), nil
	default:
		return nil, fmt.Errorf("invalid worker backend %q; expected auto, tmux, or pty", value)
	}
}

func resolveWorkerBackendPreference(cliValue string) (string, string) {
	if value := strings.TrimSpace(cliValue); value != "" {
		return strings.ToLower(value), "flag"
	}
	if value := strings.TrimSpace(os.Getenv("AGENTMUX_WORKER_BACKEND")); value != "" {
		return strings.ToLower(value), "env"
	}
	cfg, err := appconfig.Load()
	if err == nil && strings.TrimSpace(cfg.WorkerBackend) != "" {
		return strings.ToLower(strings.TrimSpace(cfg.WorkerBackend)), "config"
	}
	return "auto", "default"
}

func saveWorkerBackendConfig(value string) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "auto", "tmux", "pty", "builtin", "builtin-pty":
	default:
		fatal(fmt.Errorf("invalid worker backend %q; expected auto, tmux, or pty", value))
	}
	if value == "builtin" || value == "builtin-pty" {
		value = "pty"
	}
	cfg, err := appconfig.Load()
	if err != nil {
		fatal(err)
	}
	if cfg.WorkerToken != "" {
		if err := migrateLegacyWorkerCredential(cfg); err != nil {
			fatal(err)
		}
	}
	if err := appconfig.SaveWorkerBackend(value); err != nil {
		fatal(err)
	}
	path, _ := appconfig.Path()
	fmt.Fprintf(os.Stderr, "worker backend saved: %s (%s)\n", value, path)
}

func warnPtyFallback(tmuxErr error, source string) {
	path, _ := appconfig.Path()
	fmt.Fprintf(os.Stderr, "warning: tmux is unavailable, falling back to built-in PTY backend (%s preference).\n", source)
	fmt.Fprintln(os.Stderr, "warning: built-in PTY sessions survive Control detach/re-attach but are lost when the worker process stops.")
	fmt.Fprintf(os.Stderr, "warning: install tmux for durable sessions, then run: agentmux worker config --backend auto\n")
	if path != "" {
		fmt.Fprintf(os.Stderr, "warning: worker backend config path: %s\n", path)
	}
	if tmuxErr != nil {
		fmt.Fprintf(os.Stderr, "warning: tmux check: %s\n", compactError(tmuxErr))
	}
}

func workerIDFromAuth(auth worker.AuthResult, fallback string) string {
	if auth.DeviceID != "" {
		return auth.DeviceID
	}
	return fallback
}

func workerNameFromAuth(auth worker.AuthResult, fallback string) string {
	if auth.DeviceName != "" {
		return auth.DeviceName
	}
	return fallback
}

func runControl(ctx context.Context, args []string) {
	defer term.ResetModes(os.Stdout)
	if len(args) < 1 {
		controlUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "app":
		auth, err := resolveControlAppAuth(ctx, args[1:])
		if err != nil {
			fatal(err)
		}
		app := control.NewApp(auth.Client, auth, os.Stdin, os.Stdout)
		if err := app.Run(ctx); err != nil && err != context.Canceled {
			fatal(err)
		}
	case "login":
		runControlLogin(ctx, args[1:])
	case "workers":
		fs := controlFlags("workers", args[1:])
		client := newControlClient(ctx, fs)
		if err := client.ListWorkers(ctx, os.Stdout); err != nil {
			fatal(err)
		}
	case "list":
		fs := controlFlags("list", args[1:])
		client := newControlClient(ctx, fs)
		if err := client.ListSessions(ctx, os.Stdout); err != nil {
			fatal(err)
		}
	case "create":
		fs := flag.NewFlagSet("create", flag.ExitOnError)
		common := addControlCommon(fs)
		workerID := fs.String("worker", "", "target worker id")
		name := fs.String("name", "", "session name")
		cwd := fs.String("cwd", ".", "working directory")
		command := fs.String("command", "bash", "command to run in the session")
		_ = fs.Parse(args[1:])
		client := newControlClient(ctx, *common)
		if err := client.CreateSession(ctx, protocol.CreateSession{WorkerID: *workerID, Name: *name, CWD: *cwd, Command: *command}); err != nil {
			fatal(err)
		}
	case "send":
		fs := flag.NewFlagSet("send", flag.ExitOnError)
		common := addControlCommon(fs)
		session := fs.String("session", "", "session id worker/name")
		text := fs.String("text", "", "text to send")
		_ = fs.Parse(args[1:])
		client := newControlClient(ctx, *common)
		if err := client.SendInput(ctx, *session, *text+"\n"); err != nil {
			fatal(err)
		}
	case "stop":
		fs := flag.NewFlagSet("stop", flag.ExitOnError)
		common := addControlCommon(fs)
		session := fs.String("session", "", "session id worker/name")
		_ = fs.Parse(args[1:])
		client := newControlClient(ctx, *common)
		if err := client.StopSession(ctx, *session); err != nil {
			fatal(err)
		}
	case "attach":
		fs := flag.NewFlagSet("attach", flag.ExitOnError)
		common := addControlCommon(fs)
		session := fs.String("session", "", "session id worker/name")
		_ = fs.Parse(args[1:])
		client := newControlClient(ctx, *common)
		in, out := control.DefaultIO()
		if err := client.Attach(ctx, *session, in, out); err != nil && err != context.Canceled {
			fatal(err)
		}
	default:
		controlUsage()
		os.Exit(2)
	}
}

func resolveControlAppAuth(ctx context.Context, args []string) (control.AppAuthResult, error) {
	return resolveControlAppAuthWithLogin(ctx, args, true)
}

func resolveControlAppAuthWithLogin(ctx context.Context, args []string, login bool) (control.AppAuthResult, error) {
	fs := flag.NewFlagSet("app", flag.ExitOnError)
	common := addControlCommon(fs)
	deviceID := fs.String("device-id", "", "stable control device id")
	deviceName := fs.String("device-name", hostname(), "control device display name")
	_ = fs.Parse(args)
	if common.hub == "" && (common.token != "" || common.join != "") {
		common.hub = "http://127.0.0.1:8080"
	}
	return control.ResolveAppAuth(ctx, control.AppAuthOptions{
		HubURL: common.hub, Token: common.token, Join: common.join,
		DeviceID: *deviceID, DeviceName: *deviceName, Login: login,
	})
}

func controlHubArg(args []string) string {
	fs := flag.NewFlagSet("app", flag.ExitOnError)
	common := addControlCommon(fs)
	_ = fs.Parse(args)
	if common.hub == "" && (common.token != "" || common.join != "") {
		common.hub = "http://127.0.0.1:8080"
	}
	return common.hub
}

func runControlLogin(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	hubURL := fs.String("hub", "http://127.0.0.1:8080", "hub URL")
	deviceID := fs.String("device-id", "", "stable control device id")
	deviceName := fs.String("device-name", hostname(), "control device display name")
	_ = fs.Parse(args)
	auth, err := control.DeviceLogin(ctx, *hubURL, *deviceID, *deviceName, func(start control.DeviceStartResponse) {
		fmt.Fprintln(os.Stderr, "Open this URL to sign in:")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, start.VerificationURLComplete)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintf(os.Stderr, "Code: %s\n", start.UserCode)
		fmt.Fprintln(os.Stderr, "Waiting for browser confirmation...")
	})
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "control credential saved: tenant=%s device=%s\n", auth.TenantID, auth.DeviceID)
}

type commonControlFlags struct {
	hub   string
	token string
	join  string
}

func controlFlags(name string, args []string) commonControlFlags {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	common := addControlCommon(fs)
	_ = fs.Parse(args)
	return *common
}

func addControlCommon(fs *flag.FlagSet) *commonControlFlags {
	common := &commonControlFlags{}
	fs.StringVar(&common.hub, "hub", "", "hub URL")
	fs.StringVar(&common.token, "token", os.Getenv("AGENTMUX_TOKEN"), "shared auth token")
	fs.StringVar(&common.join, "join", "", "short-lived signal token")
	return common
}

func newControlClient(ctx context.Context, flags commonControlFlags) control.Client {
	if flags.hub == "" && (flags.token != "" || flags.join != "") {
		flags.hub = "http://127.0.0.1:8080"
	}
	auth, err := control.ResolveAppAuth(ctx, control.AppAuthOptions{
		HubURL: flags.hub, Token: flags.token, Join: flags.join,
		DeviceName: hostname(),
	})
	if err != nil {
		fatal(err)
	}
	return auth.Client
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "worker"
	}
	return name
}

func getenv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func hasJoinArg(args []string) bool {
	for _, arg := range args {
		if arg == "--join" || strings.HasPrefix(arg, "--join=") {
			return true
		}
	}
	return false
}

func flagProvided(args []string, name string) bool {
	name = strings.TrimLeft(name, "-")
	for _, arg := range args {
		arg = strings.TrimLeft(arg, "-")
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func compactError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if index := strings.Index(value, "\n"); index >= 0 {
		return value[:index]
	}
	return value
}

func workerServiceIdentity() workerservice.WorkerIdentity {
	cfg, _ := appconfig.Load()
	id := cfg.WorkerID
	if id == "" {
		if entry, ok := credentialcache.LoadLatest("worker", ""); ok {
			id = entry.DeviceID
		}
	}
	return workerservice.WorkerIdentity{ID: id}
}

func workerCredentialFromCache(hubURL, workerID string) (credentialcache.Entry, bool) {
	hubURL = strings.TrimSpace(hubURL)
	workerID = strings.TrimSpace(workerID)
	if hubURL != "" {
		if entry, ok := credentialcache.Load(hubURL, "worker", workerID); ok {
			return entry, true
		}
		return credentialcache.Entry{}, false
	}
	return credentialcache.LoadLatest("worker", workerID)
}

func migrateLegacyWorkerCredential(cfg appconfig.Config) error {
	if strings.TrimSpace(cfg.WorkerHubURL) == "" || strings.TrimSpace(cfg.WorkerToken) == "" {
		return nil
	}
	entry := credentialcache.Entry{
		HubURL: cfg.WorkerHubURL, Credential: cfg.WorkerToken,
		Role: "worker", DeviceID: cfg.WorkerID, DeviceName: cfg.WorkerName,
		UpdatedAt: time.Now().UTC(),
	}
	return credentialcache.Save(entry)
}

func acquireWorkerLock(workerID string) (string, func(), error) {
	path, err := workerservice.WorkerLockPath(workerID)
	if err != nil {
		return "", nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return "", nil, fmt.Errorf("worker %q is already running locally; stop the existing worker service or use a different --id", workerID)
	}
	workerservice.WriteLockOwner(file, os.Getpid())
	unlock := func() {
		workerservice.ClearLockOwner(file)
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
	return path, unlock, nil
}

func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return exe
}

func filepathBase(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux <hub|worker|control> [options]")
	fmt.Fprintln(os.Stderr, "       agentmux-tui [join|options]")
}

func controlUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux control <login|app|workers|list|create|send|stop|attach> [options]")
}

func workerUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux worker <join|run|start|stop|status|logs|config> [options]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
