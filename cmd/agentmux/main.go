package main

import (
	"context"
	"encoding/json"
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
	"private/agentmux/internal/updater"
	buildversion "private/agentmux/internal/version"
	"private/agentmux/internal/worker"
	"private/agentmux/internal/workerservice"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if isTUIInvocation() {
		if len(os.Args) > 1 {
			switch os.Args[1] {
			case "version":
				runVersion(os.Args[2:], "control")
				return
			case "update":
				runUpdate(ctx, os.Args[2:], "control")
				return
			}
		}
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
	case "version":
		runVersion(os.Args[2:], inferRuntimeRole())
	case "update":
		runUpdate(ctx, os.Args[2:], inferRuntimeRole())
	case "run":
		runCached(ctx, os.Args[2:])
	case "cache":
		runCache(os.Args[2:])
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
		enableControlDebug(app, defaultControlDebugFlags())
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
			common.hub = defaultControlHubURL()
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
	enableControlAppDebug(app, args)
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
	debug := addRuntimeDebug(fs, "hub")
	_ = fs.Parse(args)
	closeDebug := configureRuntimeDebug("hub", *debug)
	defer closeDebug()
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

func runVersion(args []string, role string) {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print version metadata as JSON")
	roleFlag := fs.String("role", role, "runtime role")
	_ = fs.Parse(args)
	if *jsonOut {
		data, err := buildversion.JSON(*roleFlag)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintln(os.Stdout, string(data))
		return
	}
	fmt.Fprintln(os.Stdout, buildversion.String(*roleFlag))
}

func runUpdate(ctx context.Context, args []string, defaultRole string) {
	if len(args) < 1 {
		updateUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "check":
		fs := flag.NewFlagSet("update check", flag.ExitOnError)
		repo := fs.String("repo", getenv("AGENTMUX_REPO", getenv("AGENTMUX_RELEASE_REPO", "kinboyw/agentmux")), "GitHub owner/repo")
		targetVersion := fs.String("version", "latest", "target release version or latest")
		role := fs.String("role", defaultRole, "role to update: worker, control, or hub")
		jsonOut := fs.Bool("json", false, "print result as JSON")
		_ = fs.Parse(args[1:])
		result, err := updater.Check(ctx, buildversion.Version, updater.Options{Repo: *repo, Version: *targetVersion, Role: *role})
		if err != nil {
			fatal(err)
		}
		if *jsonOut {
			writeJSONStdout(result)
			return
		}
		fmt.Fprintf(os.Stdout, "current=%s\nlatest=%s\nupdate_available=%t\nrole=%s\nasset=%s\n", result.Current, result.Latest, result.UpdateAvailable, result.Role, result.AssetName)
	case "apply":
		fs := flag.NewFlagSet("update apply", flag.ExitOnError)
		repo := fs.String("repo", getenv("AGENTMUX_REPO", getenv("AGENTMUX_RELEASE_REPO", "kinboyw/agentmux")), "GitHub owner/repo")
		targetVersion := fs.String("version", "latest", "target release version or latest")
		role := fs.String("role", defaultRole, "role to update: worker, control, or hub")
		path := fs.String("path", executablePath(), "installed binary path to replace")
		restart := fs.Bool("restart", false, "restart worker service after installing a worker update")
		jsonOut := fs.Bool("json", false, "print result as JSON")
		_ = fs.Parse(args[1:])
		result, err := updater.Install(ctx, *path, updater.Options{Repo: *repo, Version: *targetVersion, Role: *role})
		if err != nil {
			fatal(err)
		}
		if *role == "worker" && *restart {
			serviceResult, err := workerservice.Restart(ctx, result.Path, workerServiceIdentity())
			if err != nil {
				fatal(err)
			}
			if !*jsonOut {
				fmt.Fprintf(os.Stderr, "worker service restarted (%s)\n%s\n", serviceResult.Backend, serviceResult.Detail)
			}
		}
		if *jsonOut {
			writeJSONStdout(result)
			return
		}
		fmt.Fprintf(os.Stdout, "installed %s %s at %s\n", result.Role, result.Version, result.Path)
		if result.PreviousPath != "" {
			fmt.Fprintf(os.Stdout, "previous=%s\n", result.PreviousPath)
		}
	case "rollback":
		fs := flag.NewFlagSet("update rollback", flag.ExitOnError)
		jsonOut := fs.Bool("json", false, "print result as JSON")
		_ = fs.Parse(args[1:])
		result, err := updater.Rollback()
		if err != nil {
			fatal(err)
		}
		if *jsonOut {
			writeJSONStdout(result)
			return
		}
		fmt.Fprintf(os.Stdout, "rolled back %s at %s\n", result.Role, result.Path)
	default:
		updateUsage()
		os.Exit(2)
	}
}

func runCached(ctx context.Context, args []string) {
	if len(args) < 1 {
		runUsage()
		os.Exit(2)
	}
	role, targetVersion, err := parseRunTarget(args[0])
	if err != nil {
		fatal(err)
	}
	result, err := updater.DownloadToCache(ctx, updater.Options{
		Repo:    getenv("AGENTMUX_REPO", getenv("AGENTMUX_RELEASE_REPO", "kinboyw/agentmux")),
		Version: targetVersion, Role: role,
	})
	if err != nil {
		fatal(err)
	}
	execArgs := roleExecArgs(role, args[1:])
	if err := updater.Exec(ctx, result.Path, execArgs); err != nil && err != context.Canceled {
		fatal(err)
	}
}

func runCache(args []string) {
	if len(args) < 1 {
		cacheUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "prune":
		fs := flag.NewFlagSet("cache prune", flag.ExitOnError)
		jsonOut := fs.Bool("json", false, "print result as JSON")
		_ = fs.Parse(args[1:])
		result, err := updater.PruneCache()
		if err != nil {
			fatal(err)
		}
		if *jsonOut {
			writeJSONStdout(result)
			return
		}
		fmt.Fprintf(os.Stdout, "removed cache: %s (%d bytes)\n", result.Path, result.BytesRemoved)
	default:
		cacheUsage()
		os.Exit(2)
	}
}

func runWorker(ctx context.Context, args []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "join":
			runWorkerJoin(ctx, args[1:])
			return
		case "leave":
			runWorkerLeave(ctx, args[1:])
			return
		case "run":
			runWorkerForeground(ctx, args[1:])
			return
		case "start":
			runWorkerStart(ctx, args[1:]...)
			return
		case "restart":
			runWorkerRestart(ctx, args[1:]...)
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
	debug := addRuntimeDebug(fs, "worker")
	_ = fs.Parse(args)
	closeDebug := configureRuntimeDebug("worker", *debug)
	defer closeDebug()
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
	instanceID := ""
	if *join != "" {
		var err error
		instanceID, err = appconfig.EnsureWorkerInstanceID()
		if err != nil {
			fatal(err)
		}
	}
	auth, err := resolveWorkerAuthBestEffort(ctx, worker.AuthOptions{
		HubURL: *hubURL, Token: *token, Join: *join,
		DeviceID: workerID, DeviceName: *name, InstanceID: instanceID,
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
	instanceID, err := appconfig.EnsureWorkerInstanceID()
	if err != nil {
		fatal(err)
	}
	auth, err := resolveWorkerAuthBestEffort(ctx, worker.AuthOptions{
		HubURL: *hubURL, Join: *join, DeviceID: workerID, DeviceName: *name, InstanceID: instanceID,
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

func runWorkerLeave(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("leave", flag.ExitOnError)
	hubURL := fs.String("hub", "", "hub URL to clear; defaults to configured worker hub")
	id := fs.String("id", "", "worker id to clear; defaults to configured worker id")
	stop := fs.Bool("stop", true, "stop local background worker service before clearing credentials")
	_ = fs.Parse(args)
	cfg, _ := appconfig.Load()
	if *hubURL == "" {
		*hubURL = cfg.WorkerHubURL
	}
	if *id == "" {
		*id = cfg.WorkerID
	}
	if *id == "" {
		if entry, ok := credentialcache.LoadLatest("worker", ""); ok {
			*id = entry.DeviceID
			if *hubURL == "" {
				*hubURL = entry.HubURL
			}
		}
	}
	if *stop {
		if result, err := workerservice.Stop(ctx, workerservice.WorkerIdentity{ID: *id}); err == nil {
			fmt.Fprintf(os.Stderr, "worker service stopped (%s)\n%s\n", result.Backend, result.Detail)
		} else {
			slog.Default().Warn("worker service stop failed", "error", err)
		}
	}
	if err := credentialcache.Delete(*hubURL, "worker", *id); err != nil {
		fatal(err)
	}
	if err := appconfig.ClearWorkerAuth(); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stderr, "worker local join state cleared")
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

func runWorkerRestart(ctx context.Context, args ...string) {
	fs := flag.NewFlagSet("restart", flag.ExitOnError)
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
	result, err := workerservice.Restart(ctx, executablePath(), workerServiceIdentity())
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "worker service restarted (%s)\n%s\n", result.Backend, result.Detail)
}

func runWorkerStop(ctx context.Context) {
	result, err := workerservice.Stop(ctx, workerServiceIdentity())
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "worker service stopped (%s)\n%s\n", result.Backend, result.Detail)
}

func runWorkerStatus(ctx context.Context) {
	printWorkerStatusSummary(os.Stdout)
	out, err := workerservice.Status(ctx, workerServiceIdentity())
	if err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stdout, "\nservice_status:")
	fmt.Fprint(os.Stdout, out)
}

func runWorkerLogs(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	lines := fs.Int("n", 80, "number of log lines")
	follow := fs.Bool("f", false, "follow worker logs")
	_ = fs.Parse(args)
	if *follow {
		if err := workerservice.FollowLogs(ctx, *lines, os.Stdout); err != nil && err != context.Canceled {
			fatal(err)
		}
		return
	}
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
	w.Version = buildversion.Version
	w.Software.Version = buildversion.Version
	w.Software.Commit = buildversion.Commit
	w.Software.BuildTime = buildversion.BuildTime
	w.Interval = interval
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		fatal(err)
	}
}

func resolveWorkerAuthBestEffort(ctx context.Context, opts worker.AuthOptions) (worker.AuthResult, error) {
	if strings.TrimSpace(opts.Join) == "" {
		return worker.ResolveAuth(ctx, opts)
	}
	backoff := 2 * time.Second
	attempt := 1
	for {
		auth, err := worker.ResolveAuth(ctx, opts)
		if err == nil {
			return auth, nil
		}
		if !worker.IsRetryableAuthError(err) {
			return worker.AuthResult{}, err
		}
		slog.Default().Warn("worker signal exchange failed; retrying", "attempt", attempt, "retry_in", backoff.String(), "error", err)
		select {
		case <-ctx.Done():
			return worker.AuthResult{}, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		attempt++
	}
}

func printWorkerStatusSummary(out *os.File) {
	cfg, _ := appconfig.Load()
	configPath, _ := appconfig.Path()
	credentialsPath, _ := credentialcache.Path()
	logPath, _ := workerservice.LogPath()
	pidPath, _ := workerservice.PIDPath()
	lockPath := ""
	lockPID := 0
	if cfg.WorkerID != "" {
		if pid, path, ok := workerservice.LockOwnerPID(cfg.WorkerID); ok {
			lockPID = pid
			lockPath = path
		} else {
			lockPath = path
		}
	}
	resolvedBackend, backendSource := resolveWorkerBackendPreference("")
	credentialStatus := "missing"
	if entry, ok := workerCredentialFromCache(cfg.WorkerHubURL, cfg.WorkerID); ok {
		credentialStatus = "present"
		if !entry.ExpiresAt.IsZero() {
			credentialStatus = "present, expires " + entry.ExpiresAt.Format(time.RFC3339)
		}
	}
	fmt.Fprintln(out, "worker_status:")
	fmt.Fprintf(out, "  config=%s\n", configPath)
	fmt.Fprintf(out, "  credentials=%s\n", credentialsPath)
	fmt.Fprintf(out, "  hub=%s\n", cfg.WorkerHubURL)
	fmt.Fprintf(out, "  id=%s\n", cfg.WorkerID)
	fmt.Fprintf(out, "  name=%s\n", cfg.WorkerName)
	fmt.Fprintf(out, "  backend=%s\n", resolvedBackend)
	fmt.Fprintf(out, "  backend_source=%s\n", backendSource)
	fmt.Fprintf(out, "  credential=%s\n", credentialStatus)
	fmt.Fprintf(out, "  log=%s\n", logPath)
	fmt.Fprintf(out, "  pid_file=%s\n", pidPath)
	if lockPath != "" {
		if lockPID > 0 {
			fmt.Fprintf(out, "  lock=%s pid=%d\n", lockPath, lockPID)
		} else {
			fmt.Fprintf(out, "  lock=%s\n", lockPath)
		}
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
		enableControlAppDebug(app, args[1:])
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
	case "evict":
		fs := flag.NewFlagSet("evict", flag.ExitOnError)
		common := addControlCommon(fs)
		workerID := fs.String("worker", "", "worker id to evict")
		_ = fs.Parse(args[1:])
		if strings.TrimSpace(*workerID) == "" {
			fatal(fmt.Errorf("--worker is required"))
		}
		client := newControlClient(ctx, *common)
		if err := client.EvictWorker(ctx, *workerID); err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "worker evicted: %s\n", *workerID)
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
	flags := parseControlAppFlags(args)
	return control.ResolveAppAuth(ctx, control.AppAuthOptions{
		HubURL: flags.common.hub, Token: flags.common.token, Join: flags.common.join,
		DeviceID: flags.deviceID, DeviceName: flags.deviceName, Login: login,
	})
}

type parsedControlAppFlags struct {
	common     commonControlFlags
	debug      controlDebugFlags
	deviceID   string
	deviceName string
}

func parseControlAppFlags(args []string) parsedControlAppFlags {
	fs := flag.NewFlagSet("app", flag.ExitOnError)
	common := addControlCommon(fs)
	debug := addControlDebug(fs)
	deviceID := fs.String("device-id", "", "stable control device id")
	deviceName := fs.String("device-name", hostname(), "control device display name")
	_ = fs.Parse(args)
	if common.hub == "" && (common.token != "" || common.join != "") {
		common.hub = defaultControlHubURL()
	}
	return parsedControlAppFlags{common: *common, debug: *debug, deviceID: *deviceID, deviceName: *deviceName}
}

func controlHubArg(args []string) string {
	return parseControlAppFlags(args).common.hub
}

func runControlLogin(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	hubURL := fs.String("hub", defaultControlHubURL(), "hub URL")
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

type controlDebugFlags struct {
	enabled bool
	logPath string
}

type runtimeDebugFlags struct {
	enabled bool
	logPath string
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

func addControlDebug(fs *flag.FlagSet) *controlDebugFlags {
	debug := &controlDebugFlags{}
	fs.BoolVar(&debug.enabled, "debug", envBool("AGENTMUX_TUI_DEBUG") || envBool("AGENTMUX_DEBUG"), "enable TUI debug HUD and log")
	fs.StringVar(&debug.logPath, "debug-log", firstNonEmptyEnv("AGENTMUX_TUI_DEBUG_LOG", "AGENTMUX_DEBUG_LOG"), "TUI debug log path")
	return debug
}

func defaultControlDebugFlags() controlDebugFlags {
	return controlDebugFlags{
		enabled: envBool("AGENTMUX_TUI_DEBUG") || envBool("AGENTMUX_DEBUG"),
		logPath: firstNonEmptyEnv("AGENTMUX_TUI_DEBUG_LOG", "AGENTMUX_DEBUG_LOG"),
	}
}

func addRuntimeDebug(fs *flag.FlagSet, component string) *runtimeDebugFlags {
	debug := &runtimeDebugFlags{}
	envPrefix := "AGENTMUX_" + strings.ToUpper(component)
	fs.BoolVar(&debug.enabled, "debug", envBool(envPrefix+"_DEBUG") || envBool("AGENTMUX_DEBUG"), "enable debug logging")
	fs.StringVar(&debug.logPath, "debug-log", firstNonEmptyEnv(envPrefix+"_DEBUG_LOG", "AGENTMUX_DEBUG_LOG"), "debug log path")
	return debug
}

func configureRuntimeDebug(component string, flags runtimeDebugFlags) func() {
	if !flags.enabled && strings.TrimSpace(flags.logPath) == "" {
		return func() {}
	}
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	var output *os.File
	if strings.TrimSpace(flags.logPath) == "" {
		output = os.Stderr
	} else {
		file, err := os.OpenFile(strings.TrimSpace(flags.logPath), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fatal(err)
		}
		output = file
	}
	logger := slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: level})).With("component", component)
	slog.SetDefault(logger)
	slog.Default().Debug("debug logging enabled", "log", strings.TrimSpace(flags.logPath))
	return func() {
		if output != nil && output != os.Stderr {
			_ = output.Close()
		}
	}
}

func enableControlAppDebug(app *control.App, args []string) {
	flags := parseControlAppFlags(args).debug
	enableControlDebug(app, flags)
}

func enableControlDebug(app *control.App, flags controlDebugFlags) {
	if app == nil {
		return
	}
	if err := app.EnableDebug(control.AppDebugOptions{Enabled: flags.enabled, LogPath: flags.logPath}); err != nil {
		fatal(err)
	}
}

func newControlClient(ctx context.Context, flags commonControlFlags) control.Client {
	if flags.hub == "" && (flags.token != "" || flags.join != "") {
		flags.hub = defaultControlHubURL()
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

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func defaultControlHubURL() string {
	if value := strings.TrimSpace(os.Getenv("AGENTMUX_CONTROL_HUB")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("AGENTMUX_HUB")); value != "" {
		return value
	}
	if entry, ok := credentialcache.LoadLatest("control", ""); ok && strings.TrimSpace(entry.HubURL) != "" {
		return entry.HubURL
	}
	if cfg, err := appconfig.Load(); err == nil && strings.TrimSpace(cfg.WorkerHubURL) != "" {
		return cfg.WorkerHubURL
	}
	if entry, ok := credentialcache.LoadLatest("worker", ""); ok && strings.TrimSpace(entry.HubURL) != "" {
		return entry.HubURL
	}
	return "http://127.0.0.1:8080"
}

func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
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

func inferRuntimeRole() string {
	base := filepathBase(os.Args[0])
	switch {
	case strings.Contains(base, "agentmux-hub"):
		return "hub"
	case strings.Contains(base, "agentmux-tui"):
		return "control"
	default:
		return "control"
	}
}

func parseRunTarget(target string) (string, string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", "", fmt.Errorf("run target is required")
	}
	role := target
	targetVersion := "latest"
	if before, after, ok := strings.Cut(target, "@"); ok {
		role = before
		if strings.TrimSpace(after) != "" {
			targetVersion = after
		}
	}
	role = strings.ToLower(strings.TrimSpace(role))
	switch role {
	case "worker", "control", "hub":
		return role, targetVersion, nil
	default:
		return "", "", fmt.Errorf("invalid run target %q; expected worker@version, control@version, or hub@version", target)
	}
}

func roleExecArgs(role string, args []string) []string {
	out := make([]string, 0, len(args)+2)
	switch role {
	case "control":
		out = append(out, "control", "app")
	case "worker":
		out = append(out, "worker")
	case "hub":
	default:
		return args
	}
	out = append(out, args...)
	return out
}

func writeJSONStdout(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stdout, string(data))
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
	fmt.Fprintln(os.Stderr, "usage: agentmux <hub|worker|control|version|update|run|cache> [options]")
	fmt.Fprintln(os.Stderr, "       agentmux-tui [join|options]")
}

func controlUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux control <login|app|workers|evict|list|create|send|stop|attach> [options]")
}

func workerUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux worker <join|leave|run|start|restart|stop|status|logs|config> [options]")
}

func updateUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux update <check|apply|rollback> [options]")
}

func runUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux run <worker|control|hub>[@version] [role options]")
}

func cacheUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux cache <prune> [options]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
