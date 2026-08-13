package runtime

import (
	"context"
	"errors"
	"fmt"
	"image"

	"github.com/tomharris/lw-manager/internal/capture"
)

// Capturer persists an already-captured frame. *capture.Service satisfies
// it; tests use a fake.
type Capturer interface {
	Record(ctx context.Context, accountID int64, img image.Image, screenID *string) (capture.Result, error)
}

// Capture verifies the device shows the named screen, then persists that
// frame with the screen id attached — the provenance every OCR-derived
// number must trace back to (invariant #5). The screenshot id is remembered
// for the task_runs row.
func (c *Ctx) Capture(ctx context.Context, screenID string) (int64, error) {
	if err := c.ks.Check(ctx); err != nil {
		return 0, err
	}
	if c.cap == nil {
		return 0, errors.New("runtime: no capturer configured")
	}
	frame, err := c.verifyScreen(ctx, screenID)
	if err != nil {
		return 0, err
	}
	res, err := c.cap.Record(ctx, c.accountID, frame, &screenID)
	if err != nil {
		return 0, fmt.Errorf("runtime: recording capture of %q for account %d: %w", screenID, c.accountID, err)
	}
	c.screenshotIDs = append(c.screenshotIDs, res.ScreenshotID)
	return res.ScreenshotID, nil
}

// StoreFrame persists an already-held frame under screenID, without taking a
// fresh screenshot or re-verifying it. Pair it with VerifyFrame: this method
// trusts the caller already confirmed img shows screenID.
//
// It exists for callers that already hold a screen-verified image — such as
// scrollCapture's offset measurement — and would otherwise pay for a second,
// separate screenshot just to store it. That second screenshot is not free:
// it is not merely slower, it is a different set of pixels than the ones any
// prior measurement was computed against, on any screen that is not
// perfectly static between the two captures.
func (c *Ctx) StoreFrame(ctx context.Context, screenID string, img image.Image) (int64, error) {
	if err := c.ks.Check(ctx); err != nil {
		return 0, err
	}
	if c.cap == nil {
		return 0, errors.New("runtime: no capturer configured")
	}
	res, err := c.cap.Record(ctx, c.accountID, img, &screenID)
	if err != nil {
		return 0, fmt.Errorf("runtime: recording capture of %q for account %d: %w", screenID, c.accountID, err)
	}
	c.screenshotIDs = append(c.screenshotIDs, res.ScreenshotID)
	return res.ScreenshotID, nil
}

// ScreenshotIDs returns every screenshot this Ctx has recorded, for the
// task_runs audit row.
func (c *Ctx) ScreenshotIDs() []int64 {
	out := make([]int64, len(c.screenshotIDs))
	copy(out, c.screenshotIDs)
	return out
}

// CaptureFrameRef is one frame belonging to a multi-frame capture run — the
// runtime-side shape a task builds from []ScrolledFrame before handing it to
// RecordCapture. It mirrors db.CaptureFrame's writable fields without this
// package importing internal/db, the same boundary Capturer already draws
// via capture.Result.
//
// This type and CaptureRecorder are Task 8's addition, ahead of Task 10's
// database-backed implementation: roster_capture needed something to record
// through and to fake in its tests, and this is the minimal interface that
// lets it compile and be tested without the storage layer existing yet.
// Task 10 should implement CaptureRecorder against *db.Pool (creating the
// capture row, calling AddCaptureFrame per frame, then FinishCapture) rather
// than adding a second recording path.
type CaptureFrameRef struct {
	ScreenshotID int64
	Seq          int
	OffsetPx     int
	GroupKey     string
}

// CaptureRecorder persists one multi-frame capture run: its route, every
// frame in scroll order, and whether the run reached a proven end (a bottom
// the scroll loop actually measured, not just gave up at a frame cap).
// Task 10's database-backed implementation satisfies it; tests use a fake.
type CaptureRecorder interface {
	RecordCapture(ctx context.Context, accountID int64, route string, frames []CaptureFrameRef, complete bool) error
}

// RecordCapture persists a capture run assembled from many scrolled frames —
// the roster and VS-ranking routes' counterpart to Capture, which only ever
// stores one screenshot at a time.
func (c *Ctx) RecordCapture(ctx context.Context, route string, frames []CaptureFrameRef, complete bool) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	if c.capRec == nil {
		return errors.New("runtime: no capture recorder configured")
	}
	if err := c.capRec.RecordCapture(ctx, c.accountID, route, frames, complete); err != nil {
		return fmt.Errorf("runtime: recording %q capture for account %d: %w", route, c.accountID, err)
	}
	return nil
}
