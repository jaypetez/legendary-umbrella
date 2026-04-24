package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jaysonpetersen/legendary-umbrella/internal/agent"
)

const usage = `connect-agent — device agent for the Connect service

Usage:
  agent enroll [--server URL] [--config PATH]
  agent run    [--config PATH] [-v]
  agent status [--config PATH]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "enroll":
		os.Exit(cmdEnroll(args))
	case "run":
		os.Exit(cmdRun(args))
	case "status":
		os.Exit(cmdStatus(args))
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func cmdEnroll(args []string) int {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", envOr("CONNECT_SERVER", "http://localhost:8080"), "Signaling server URL")
	cfgPath := fs.String("config", "", "Config file path (default per-OS)")
	_ = fs.Parse(args)

	path, err := resolveConfigPath(*cfgPath)
	if err != nil {
		slog.Error("resolve config path", "err", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := agent.Enroll(ctx, agent.EnrollOptions{
		ServerURL:  *server,
		ConfigPath: path,
		Stdout:     os.Stdout,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "enroll failed:", err)
		return 1
	}
	return 0
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "Config file path (default per-OS)")
	verbose := fs.Bool("v", false, "Verbose logging")
	_ = fs.Parse(args)

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	path, err := resolveConfigPath(*cfgPath)
	if err != nil {
		slog.Error("resolve config path", "err", err)
		return 1
	}
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, agent.ErrNotEnrolled.Error())
			return 1
		}
		fmt.Fprintln(os.Stderr, "load config:", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err = agent.Run(ctx, agent.RunOptions{Config: cfg})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "run ended:", err)
		return 1
	}
	return 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", "", "Config file path (default per-OS)")
	_ = fs.Parse(args)

	path, err := resolveConfigPath(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve config path:", err)
		return 1
	}
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("not enrolled. Run `agent enroll`.")
			return 1
		}
		fmt.Fprintln(os.Stderr, "load config:", err)
		return 1
	}
	fmt.Println("server:   ", cfg.ServerURL)
	fmt.Println("device id:", cfg.DeviceID)
	fmt.Println("name:     ", cfg.DeviceName)
	fmt.Println("config:   ", path)
	return 0
}

func resolveConfigPath(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return agent.DefaultConfigPath()
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}
