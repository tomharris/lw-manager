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
