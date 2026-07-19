package capture

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/transport"
)

// fakeStore records what the capture path wrote, with no Postgres involved.
type fakeStore struct {
	target      db.CaptureTarget
	targetErr   error
	screenshots []db.Screenshot
	upserts     []db.Device
	nextID      int64
}

func (f *fakeStore) CaptureTargetByAccount(_ context.Context, _ int64) (db.CaptureTarget, error) {
	if f.targetErr != nil {
		return db.CaptureTarget{}, f.targetErr
	}
	return f.target, nil
}

func (f *fakeStore) InsertScreenshot(_ context.Context, accountID int64, key, sum string, screenID *string) (db.Screenshot, error) {
	f.nextID++
	s := db.Screenshot{ID: f.nextID, AccountID: accountID, ObjectKey: key, SHA256: sum, ScreenID: screenID}
	f.screenshots = append(f.screenshots, s)
	return s, nil
}

func (f *fakeStore) UpsertDevice(_ context.Context, serial, tr string, w, h int) (db.Device, error) {
	d := db.Device{ID: 1, Serial: serial, Transport: tr, ResolutionW: w, ResolutionH: h}
	f.upserts = append(f.upserts, d)
	return d, nil
}

func frame(w, h int, tag uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: tag, G: tag, B: tag, A: 255})
		}
	}
	return img
}

func newHarness(t *testing.T, frames ...image.Image) (*Service, *fakeStore, blob.Store) {
	t.Helper()

	store := &fakeStore{target: db.CaptureTarget{
		AccountID: 7, Nickname: "testalt", Role: "alliance_data", Enabled: true,
		Package: transport.DefaultPackage, DeviceID: 1, Serial: "emulator-5554",
		Transport: "adb", ResolutionW: 100, ResolutionH: 200,
	}}

	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore(): %v", err)
	}

	rt, err := transport.NewReplayTransportFromImages(frames...)
	if err != nil {
		t.Fatalf("NewReplayTransportFromImages(): %v", err)
	}

	factory := func(_ context.Context, _ db.CaptureTarget) (transport.Transport, error) {
		return rt, nil
	}
	return New(store, blobs, factory), store, blobs
}

func TestCaptureWritesBlobAndRow(t *testing.T) {
	ctx := context.Background()
	svc, store, blobs := newHarness(t, frame(100, 200, 42))

	res, err := svc.Capture(ctx, 7)
	if err != nil {
		t.Fatalf("Capture(): %v", err)
	}

	if res.ScreenshotID == 0 {
		t.Error("ScreenshotID = 0, want a persisted row id")
	}
	if res.Bytes == 0 {
		t.Error("Bytes = 0, want encoded PNG size")
	}
	if res.Deduplicated {
		t.Error("Deduplicated = true on the first capture of this content")
	}
	if len(store.screenshots) != 1 {
		t.Fatalf("wrote %d screenshot rows, want 1", len(store.screenshots))
	}
	if store.screenshots[0].ScreenID != nil {
		t.Error("screen_id was set, but nothing recognizes screens at M0")
	}

	// The blob must actually be retrievable and match its digest.
	if _, err := blob.GetContent(ctx, blobs, res.ObjectKey, res.SHA256); err != nil {
		t.Fatalf("stored blob failed verification: %v", err)
	}
}

// Identical pixels: one blob, two observations. Collapsing the second row
// would silently under-report participation.
func TestCaptureDeduplicatesBlobButNotRows(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newHarness(t, frame(100, 200, 42))

	first, err := svc.Capture(ctx, 7)
	if err != nil {
		t.Fatalf("first Capture(): %v", err)
	}
	second, err := svc.Capture(ctx, 7)
	if err != nil {
		t.Fatalf("second Capture(): %v", err)
	}

	if second.SHA256 != first.SHA256 || second.ObjectKey != first.ObjectKey {
		t.Errorf("identical frames produced different keys: %q vs %q", first.ObjectKey, second.ObjectKey)
	}
	if !second.Deduplicated {
		t.Error("second capture of identical content was not flagged as deduplicated")
	}
	if second.ScreenshotID == first.ScreenshotID {
		t.Error("second capture reused the first row; each capture is a distinct observation")
	}
	if len(store.screenshots) != 2 {
		t.Errorf("wrote %d rows, want 2", len(store.screenshots))
	}
}

func TestCaptureDistinctFramesDistinctBlobs(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newHarness(t, frame(100, 200, 1), frame(100, 200, 2))

	first, err := svc.Capture(ctx, 7)
	if err != nil {
		t.Fatalf("first Capture(): %v", err)
	}
	second, err := svc.Capture(ctx, 7)
	if err != nil {
		t.Fatalf("second Capture(): %v", err)
	}
	if first.SHA256 == second.SHA256 {
		t.Error("different frames hashed identically")
	}
}

// Disabled is the per-account kill switch. It must stop a capture outright.
func TestCaptureRefusesDisabledAccount(t *testing.T) {
	svc, store, _ := newHarness(t, frame(100, 200, 1))
	store.target.Enabled = false

	if _, err := svc.Capture(context.Background(), 7); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("error = %v, want ErrAccountDisabled", err)
	}
	if len(store.screenshots) != 0 {
		t.Error("a disabled account still produced a screenshot row")
	}
}

// A resized emulator must reconcile, or every normalized coordinate derived
// from the stale resolution is quietly wrong.
func TestCaptureReconcilesChangedResolution(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newHarness(t, frame(720, 1600, 1))

	if _, err := svc.Capture(ctx, 7); err != nil {
		t.Fatalf("Capture(): %v", err)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("recorded %d device upserts, want 1", len(store.upserts))
	}
	if got := store.upserts[0]; got.ResolutionW != 720 || got.ResolutionH != 1600 {
		t.Errorf("upserted resolution %dx%d, want 720x1600", got.ResolutionW, got.ResolutionH)
	}
}

func TestCaptureSkipsUpsertWhenResolutionMatches(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newHarness(t, frame(100, 200, 1))

	if _, err := svc.Capture(ctx, 7); err != nil {
		t.Fatalf("Capture(): %v", err)
	}
	if len(store.upserts) != 0 {
		t.Errorf("recorded %d device upserts for an unchanged resolution, want 0", len(store.upserts))
	}
}

func TestCapturePropagatesLookupFailure(t *testing.T) {
	svc, store, _ := newHarness(t, frame(100, 200, 1))
	store.targetErr = db.ErrNotFound

	if _, err := svc.Capture(context.Background(), 7); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("error = %v, want db.ErrNotFound", err)
	}
}
