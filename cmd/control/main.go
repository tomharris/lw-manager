// Command control is the control plane: migrations today, API and scheduler
// as later milestones land.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "control: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: control <command> [flags]

commands:
  migrate   apply database migrations
  serve     run the control-plane HTTP server
  ingest    run the analytics ingest pass for one capture
  alliance  declare or show the bot's one tracked alliance identity
  pause     set the global kill switch
  resume    clear the global kill switch
`)
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("a command is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logging.Setup(cfg.Log)

	switch os.Args[1] {
	case "migrate":
		if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
		slog.Info("migrations applied")
		return nil
	case "serve":
		return runServe(ctx, cfg, os.Args[2:])
	case "ingest":
		return runIngestCmd(ctx, cfg, os.Args[2:])
	case "alliance":
		return runAllianceCmd(ctx, cfg, os.Args[2:])
	case "pause":
		return runPause(ctx, cfg, os.Args[2:])
	case "resume":
		return runResume(ctx, cfg)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func runServe(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	mux := http.NewServeMux()

	// Health checks the database too: a control plane that cannot reach
	// Postgres is not healthy, however well its own process is running.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status := map[string]string{"status": "ok", "database": "ok"}
		code := http.StatusOK
		if err := pool.Ping(ctx); err != nil {
			// The detail stays in the logs. pgx ping errors embed the
			// username, database name, host and port, and /healthz is
			// unauthenticated — that is free reconnaissance for anyone who
			// can reach the port.
			slog.Error("healthz database ping failed", "error", err)
			status["status"] = "degraded"
			status["database"] = "unavailable"
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("control plane listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("control: serving: %w", err)
	}
	return nil
}

func runPause(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("pause", flag.ExitOnError)
	reason := fs.String("reason", "", "why the fleet is being paused (required, it ends up in every ErrPaused)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reason == "" {
		fs.Usage()
		return fmt.Errorf("--reason is required: future-you wants to know why everything stopped")
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.SetPauseAll(ctx, true, *reason); err != nil {
		return err
	}
	fmt.Printf("pause_all=true reason=%q\n", *reason)
	return nil
}

func runResume(ctx context.Context, cfg config.Config) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.SetPauseAll(ctx, false, ""); err != nil {
		return err
	}
	fmt.Println("pause_all=false")
	return nil
}
