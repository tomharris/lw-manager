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

// ScreenshotIDs returns every screenshot this Ctx has recorded, for the
// task_runs audit row.
func (c *Ctx) ScreenshotIDs() []int64 {
	out := make([]int64, len(c.screenshotIDs))
	copy(out, c.screenshotIDs)
	return out
}
