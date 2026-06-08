package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"private/agentmux/internal/control"
	"private/agentmux/internal/hub"
	"private/agentmux/internal/protocol"
	"private/agentmux/internal/term"
	"private/agentmux/internal/tmux"
	"private/agentmux/internal/worker"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

func runHub(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	token := fs.String("token", os.Getenv("AGENTMUX_TOKEN"), "shared auth token")
	_ = fs.Parse(args)
	server := hub.New(*addr, *token, slog.Default())
	if err := server.ListenAndServe(ctx); err != nil {
		fatal(err)
	}
}

func runWorker(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	hubURL := fs.String("hub", "ws://127.0.0.1:8080", "hub URL")
	token := fs.String("token", os.Getenv("AGENTMUX_TOKEN"), "shared auth token")
	join := fs.String("join", "", "short-lived signal token")
	id := fs.String("id", "", "stable worker id")
	name := fs.String("name", hostname(), "worker display name")
	interval := fs.Duration("interval", time.Second, "terminal capture interval")
	_ = fs.Parse(args)
	workerID := *id
	if workerID == "" {
		workerID = *name
	}
	if *join != "" {
		credential, deviceID, err := worker.ExchangeSignal(ctx, *hubURL, *join, workerID, *name)
		if err != nil {
			fatal(err)
		}
		*token = credential
		workerID = deviceID
		slog.Default().Info("worker signal exchanged", "device_id", deviceID)
	}
	w := worker.New(*hubURL, *token, workerID, *name, tmux.New(nil), slog.Default())
	w.Interval = *interval
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		fatal(err)
	}
}

func runControl(ctx context.Context, args []string) {
	defer term.ResetModes(os.Stdout)
	if len(args) < 1 {
		controlUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "app":
		fs := flag.NewFlagSet("app", flag.ExitOnError)
		common := addControlCommon(fs)
		deviceID := fs.String("device-id", "", "stable control device id")
		deviceName := fs.String("device-name", hostname(), "control device display name")
		_ = fs.Parse(args[1:])
		auth, err := control.ResolveAppAuth(ctx, control.AppAuthOptions{
			HubURL: common.hub, Token: common.token, Join: common.join,
			DeviceID: *deviceID, DeviceName: *deviceName,
		})
		if err != nil {
			fatal(err)
		}
		app := control.NewApp(auth.Client, auth, os.Stdin, os.Stdout)
		if err := app.Run(ctx); err != nil && err != context.Canceled {
			fatal(err)
		}
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
		name := fs.String("name", "", "tmux session name")
		cwd := fs.String("cwd", ".", "working directory")
		command := fs.String("command", "bash", "command to run in tmux")
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
	fs.StringVar(&common.hub, "hub", "http://127.0.0.1:8080", "hub URL")
	fs.StringVar(&common.token, "token", os.Getenv("AGENTMUX_TOKEN"), "shared auth token")
	fs.StringVar(&common.join, "join", "", "short-lived signal token")
	return common
}

func newControlClient(ctx context.Context, flags commonControlFlags) control.Client {
	if flags.join == "" {
		return control.New(flags.hub, flags.token)
	}
	client := control.New(flags.hub, "")
	credential, err := client.ExchangeSignal(ctx, flags.join, "control", "", hostname())
	if err != nil {
		fatal(err)
	}
	return control.New(flags.hub, credential)
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "worker"
	}
	return name
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux <hub|worker|control> [options]")
}

func controlUsage() {
	fmt.Fprintln(os.Stderr, "usage: agentmux control <app|workers|list|create|send|stop|attach> [options]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
