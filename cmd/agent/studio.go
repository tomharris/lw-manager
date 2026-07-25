package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/studio"
	"github.com/tomharris/lw-manager/internal/transport"
)

func runStudio(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("studio", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address; a non-loopback bind requires a token")
	token := fs.String("token", "", "shared secret; generated and printed when empty")
	root := fs.String("corpus", "fixtures/corpus", "corpus root directory")
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	refHeight := fs.Int("ref-height", 0, "template library reference height in pixels; defaults to the attached device's height")
	serial := fs.String("serial", "", "device serial for capture-now; optional when exactly one device is attached")
	noDevice := fs.Bool("no-device", false, "run without a device: labelling and cropping only")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *token == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("generating a studio token: %w", err)
		}
		*token = hex.EncodeToString(buf)
	}

	var tr transport.Transport
	if !*noDevice {
		resolved, err := resolveSerial(ctx, cfg.ADBPath, *serial)
		if err != nil {
			return fmt.Errorf("%w (pass --no-device to label without a phone)", err)
		}
		adb, err := transport.NewADBTransport(ctx, transport.ADBOptions{ADBPath: cfg.ADBPath, Serial: resolved})
		if err != nil {
			return fmt.Errorf("opening device %s: %w", resolved, err)
		}
		defer adb.Close()
		tr = adb
		if *refHeight == 0 {
			*refHeight = adb.Resolution().Y
		}
	}
	if *refHeight == 0 {
		return fmt.Errorf("--ref-height is required with --no-device")
	}

	srv, err := studio.New(studio.Options{
		Corpus:       corpus.New(*root),
		Transport:    tr,
		ManifestPath: *manifest,
		RefHeight:    *refHeight,
		Token:        *token,
		Logger:       slog.Default(),
	})
	if err != nil {
		return err
	}

	// The URL goes to stdout so it can be piped or copied; everything else is
	// a log line on stderr.
	scheme := "http://" + *addr
	if studio.RequireToken(*addr) {
		slog.Warn("studio is bound to a non-loopback address", "addr", *addr)
	}
	fmt.Printf("%s/?t=%s\n", scheme, *token)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("studio server on %s: %w", *addr, err)
	}
	return nil
}
