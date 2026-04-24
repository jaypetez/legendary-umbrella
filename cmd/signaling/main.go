package main

import (
	"context"
	"embed"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jaysonpetersen/legendary-umbrella/internal/signaling"
)

//go:embed web/*
var webFiles embed.FS

func main() {
	var (
		addr      = flag.String("addr", ":8080", "HTTP listen address")
		dbPath    = flag.String("db", "./data/signaling.db", "SQLite database path")
		publicURL = flag.String("public-url", envOr("PUBLIC_URL", ""), "Public base URL; derived from request Host if empty")
		adminTok  = flag.String("admin-token", envOr("SIG_ADMIN_TOKEN", ""), "If set, browser API requires X-Admin-Token header")
		verbose   = flag.Bool("v", false, "Verbose (debug) logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := os.MkdirAll("./data", 0o755); err != nil {
		slog.Error("mkdir data", "err", err)
		os.Exit(1)
	}

	store, err := signaling.OpenStore(*dbPath)
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	signaling.SetWebFS(webFiles, "web")

	srv := signaling.NewServer(signaling.Config{
		Addr:       *addr,
		PublicURL:  *publicURL,
		AdminToken: *adminTok,
	}, store)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}
