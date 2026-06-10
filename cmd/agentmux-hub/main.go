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

	"private/agentmux/internal/hub"
	"private/agentmux/internal/updater"
	buildversion "private/agentmux/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version":
			runVersion(args[1:])
			return
		case "update":
			runUpdate(ctx, args[1:])
			return
		}
	}
	if len(args) > 0 && args[0] == "hub" {
		args = args[1:]
	}
	runHub(ctx, args)
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
	server, err := hub.NewWithOptions(hub.ServerOptions{
		Addr: *addr, Token: *token, PublicURL: *publicURL,
		ReleaseRepo: *releaseRepo, Logger: slog.Default(), AuthStore: authStore,
	})
	if err != nil {
		fatal(err)
	}
	if err := server.ListenAndServe(ctx); err != nil {
		fatal(err)
	}
}

func runVersion(args []string) {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print version metadata as JSON")
	_ = fs.Parse(args)
	if *jsonOut {
		data, err := buildversion.JSON("hub")
		if err != nil {
			fatal(err)
		}
		fmt.Fprintln(os.Stdout, string(data))
		return
	}
	fmt.Fprintln(os.Stdout, buildversion.String("hub"))
}

func runUpdate(ctx context.Context, args []string) {
	if len(args) < 1 {
		updateUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "check":
		fs := flag.NewFlagSet("update check", flag.ExitOnError)
		repo := fs.String("repo", getenv("AGENTMUX_REPO", getenv("AGENTMUX_RELEASE_REPO", "kinboyw/agentmux")), "GitHub owner/repo")
		targetVersion := fs.String("version", "latest", "target release version or latest")
		jsonOut := fs.Bool("json", false, "print result as JSON")
		_ = fs.Parse(args[1:])
		result, err := updater.Check(ctx, buildversion.Version, updater.Options{Repo: *repo, Version: *targetVersion, Role: "hub"})
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
		path := fs.String("path", executablePath(), "installed binary path to replace")
		jsonOut := fs.Bool("json", false, "print result as JSON")
		_ = fs.Parse(args[1:])
		result, err := updater.Install(ctx, *path, updater.Options{Repo: *repo, Version: *targetVersion, Role: "hub"})
		if err != nil {
			fatal(err)
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

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return exe
}

func writeJSONStdout(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stdout, string(data))
}

func updateUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux-hub update <check|apply|rollback> [options]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
