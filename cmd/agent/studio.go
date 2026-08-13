package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/studio"
	"github.com/tomharris/lw-manager/internal/transport"
)

// reviewConnectTimeout bounds how long agent studio will wait to reach
// Postgres before giving up on the review surface. Corpus labelling is
// studio's original job and needs no database, so an unreachable or slow
// Postgres must degrade studio to that, not hang or fail its startup.
const reviewConnectTimeout = 3 * time.Second

// connectReviewStore attempts to reach the database and blob store the
// review UI needs, following the exact construction cmd/control already
// uses (db.Connect, blob.New) rather than a second path. It is best-effort
// by design: any failure -- unreachable, misconfigured, simply not running
// -- is logged and swallowed, returning (nil, nil), never an error, because
// the caller must still start studio for corpus labelling either way. The
// returned *db.Pool is the caller's to Close.
func connectReviewStore(ctx context.Context, cfg config.Config) (*db.Pool, blob.Store) {
	connectCtx, cancel := context.WithTimeout(ctx, reviewConnectTimeout)
	defer cancel()

	pool, err := db.Connect(connectCtx, cfg.DatabaseURL)
	if err != nil {
		slog.Warn("studio: review unavailable, could not reach the database; serving corpus routes only", "err", err)
		return nil, nil
	}
	blobs, err := blob.New(connectCtx, cfg.Blob)
	if err != nil {
		pool.Close()
		slog.Warn("studio: review unavailable, could not reach the blob store; serving corpus routes only", "err", err)
		return nil, nil
	}
	return pool, blobs
}

func runStudio(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("studio", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address; a non-loopback bind requires a token")
	token := fs.String("token", "", "shared secret; generated and printed when empty")
	root := fs.String("corpus", "fixtures/corpus", "corpus root directory")
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	refHeight := fs.Int("ref-height", 0, "template library reference height in pixels; defaults to the attached device's height")
	serial := fs.String("serial", "", "device serial for capture-now; optional when exactly one device is attached")
	noDevice := fs.Bool("no-device", false, "run without a device: labelling and cropping only")
	noReview := fs.Bool("no-review", false, "run without the review store: never attempt to reach the database, labelling and cropping only")
	pkg := fs.String("package", transport.DefaultPackage, "game package name, for the metadata stamp on capture-now frames")
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
	var meta corpus.Meta
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
		res := adb.Resolution()
		if *refHeight == 0 {
			*refHeight = res.Y
		}
		model, gameVersion, err := transport.DeviceProps(ctx, cfg.ADBPath, resolved, *pkg)
		if err != nil {
			return err
		}
		meta = corpus.Meta{Width: res.X, Height: res.Y, Device: model, GameVersion: gameVersion}
	}
	if *refHeight == 0 {
		return fmt.Errorf("--ref-height is required with --no-device")
	}

	opts := studio.Options{
		Corpus:       corpus.New(*root),
		Transport:    tr,
		ManifestPath: *manifest,
		RefHeight:    *refHeight,
		Token:        *token,
		Logger:       slog.Default(),
		Meta:         meta,
	}
	// The review store is genuinely optional: a nil *db.Pool assigned to the
	// studio.ReviewStore interface field would be a non-nil interface
	// holding a nil pointer, which studio's own `s.review != nil` route-
	// registration check cannot tell apart from a real store -- so this only
	// sets Review/Blobs at all when connectReviewStore actually found one.
	if !*noReview {
		if pool, blobs := connectReviewStore(ctx, cfg); pool != nil {
			defer pool.Close()
			opts.Review = pool
			opts.Blobs = blobs
			slog.Info("studio: review routes enabled")
		}
	} else {
		slog.Info("studio: review routes disabled by --no-review")
	}

	srv, err := studio.New(opts)
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
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("studio server on %s: %w", *addr, err)
	}
	return nil
}
