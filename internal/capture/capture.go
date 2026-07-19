// Package capture records one screenshot: device to object store to database.
//
// It is deliberately thin. Everything it needs is behind an interface, so the
// whole path is exercisable with no device, no MinIO, and no Postgres — the
// same discipline the vision milestone will depend on.
package capture

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"time"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/transport"
)

// ErrAccountDisabled reports an attempt to drive a disabled account. Disabled
// is the per-account kill switch, so this must fail rather than warn.
var ErrAccountDisabled = errors.New("capture: account is disabled")

// Store is the slice of the database the capture path needs. Narrow by
// design: *db.Pool satisfies it, and so does a test fake.
type Store interface {
	CaptureTargetByAccount(ctx context.Context, accountID int64) (db.CaptureTarget, error)
	InsertScreenshot(ctx context.Context, accountID int64, objectKey, sha256 string, screenID *string) (db.Screenshot, error)
	UpsertDevice(ctx context.Context, serial, transportName string, w, h int) (db.Device, error)
}

// Factory opens a transport for a resolved target. Production passes an adb
// factory; tests pass one that returns a ReplayTransport.
type Factory func(ctx context.Context, target db.CaptureTarget) (transport.Transport, error)

// Service performs captures.
type Service struct {
	store   Store
	blobs   blob.Store
	factory Factory
	log     *slog.Logger
}

// New builds a capture service.
func New(store Store, blobs blob.Store, factory Factory) *Service {
	return &Service{
		store:   store,
		blobs:   blobs,
		factory: factory,
		log:     slog.Default().With("component", "capture"),
	}
}

// Result describes a completed capture.
type Result struct {
	ScreenshotID int64
	AccountID    int64
	ObjectKey    string
	SHA256       string
	Bytes        int
	Resolution   image.Point
	CapturedAt   time.Time
	// Deduplicated is true when these exact bytes were already stored. The
	// screenshot row is still written: each capture is a distinct observation
	// even when the pixels are identical.
	Deduplicated bool
}

// Capture takes one screenshot for an account and records it.
func (s *Service) Capture(ctx context.Context, accountID int64) (Result, error) {
	target, err := s.store.CaptureTargetByAccount(ctx, accountID)
	if err != nil {
		return Result{}, err
	}
	if !target.Enabled {
		return Result{}, fmt.Errorf("capture: account %d (%s): %w", accountID, target.Nickname, ErrAccountDisabled)
	}

	tr, err := s.factory(ctx, target)
	if err != nil {
		return Result{}, fmt.Errorf("capture: opening transport for device %s: %w", target.Serial, err)
	}
	defer tr.Close()

	// Reconcile the device's real resolution on every capture. An emulator can
	// be resized between runs, and a stale resolution would silently skew
	// every normalized coordinate derived from it.
	res := tr.Resolution()
	if res != (image.Point{X: target.ResolutionW, Y: target.ResolutionH}) {
		if _, err := s.store.UpsertDevice(ctx, target.Serial, target.Transport, res.X, res.Y); err != nil {
			return Result{}, err
		}
		s.log.Info("device resolution updated",
			"serial", target.Serial,
			"from", fmt.Sprintf("%dx%d", target.ResolutionW, target.ResolutionH),
			"to", fmt.Sprintf("%dx%d", res.X, res.Y))
	}

	img, err := tr.Screenshot(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("capture: screenshotting %s: %w", target.Serial, err)
	}

	data, err := encodePNG(img)
	if err != nil {
		return Result{}, err
	}

	sum := blob.Sum(data)
	key := blob.Key(sum)
	existed, err := s.blobs.Exists(ctx, key)
	if err != nil {
		return Result{}, fmt.Errorf("capture: checking blob %s: %w", key, err)
	}

	if _, _, err := blob.PutContent(ctx, s.blobs, data); err != nil {
		return Result{}, err
	}

	// screen_id stays nil at M0: nothing recognizes screens yet.
	row, err := s.store.InsertScreenshot(ctx, accountID, key, sum, nil)
	if err != nil {
		return Result{}, err
	}

	s.log.Info("captured",
		"account", accountID,
		"nickname", target.Nickname,
		"serial", target.Serial,
		"screenshot_id", row.ID,
		"bytes", len(data),
		"deduplicated", existed)

	return Result{
		ScreenshotID: row.ID,
		AccountID:    accountID,
		ObjectKey:    key,
		SHA256:       sum,
		Bytes:        len(data),
		Resolution:   res,
		CapturedAt:   row.CapturedAt,
		Deduplicated: existed,
	}, nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytesBuffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("capture: encoding png: %w", err)
	}
	return buf.b, nil
}

// bytesBuffer is a minimal append-only writer, avoiding a bytes.Buffer import
// solely to collect encoder output.
type bytesBuffer struct{ b []byte }

func (w *bytesBuffer) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// ADBFactory returns a Factory that opens real adb transports.
func ADBFactory(adbPath string) Factory {
	return func(ctx context.Context, target db.CaptureTarget) (transport.Transport, error) {
		return transport.NewADBTransport(ctx, transport.ADBOptions{
			ADBPath: adbPath,
			Serial:  target.Serial,
			Package: target.Package,
		})
	}
}
