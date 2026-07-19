// Command agent drives devices: capture, and later, task execution.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/capture"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/logging"
	"github.com/tomharris/lw-manager/internal/transport"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: agent <command> [flags]

commands:
  capture   capture one screenshot for an account
  devices   list attached adb devices
`)
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("a command is required")
	}

	// Ctrl-C must reach in-flight adb calls, which run under this context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logging.Setup(cfg.Log)

	switch os.Args[1] {
	case "capture":
		return runCapture(ctx, cfg, os.Args[2:])
	case "devices":
		return runDevices(ctx, cfg)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func runCapture(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	accountID := fs.Int64("account", 0, "account id to capture for (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *accountID == 0 {
		fs.Usage()
		return fmt.Errorf("--account is required")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		return err
	}

	svc := capture.New(pool, blobs, capture.ADBFactory(cfg.ADBPath))
	res, err := svc.Capture(ctx, *accountID)
	if err != nil {
		return err
	}

	// stdout stays machine-readable; all logging goes to stderr.
	fmt.Printf("screenshot_id=%d key=%s sha256=%s bytes=%d resolution=%dx%d deduplicated=%t\n",
		res.ScreenshotID, res.ObjectKey, res.SHA256, res.Bytes,
		res.Resolution.X, res.Resolution.Y, res.Deduplicated)
	return nil
}

func runDevices(ctx context.Context, cfg config.Config) error {
	serials, err := transport.ListDevices(ctx, cfg.ADBPath)
	if err != nil {
		return err
	}
	if len(serials) == 0 {
		fmt.Println("no devices attached")
		return nil
	}
	for _, s := range serials {
		t, err := transport.NewADBTransport(ctx, transport.ADBOptions{ADBPath: cfg.ADBPath, Serial: s})
		if err != nil {
			fmt.Printf("%s\terror: %v\n", s, err)
			continue
		}
		res := t.Resolution()
		fmt.Printf("%s\t%dx%d\n", s, res.X, res.Y)
		t.Close()
	}
	return nil
}
