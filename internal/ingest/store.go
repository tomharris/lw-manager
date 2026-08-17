package ingest

import (
	"context"
	"fmt"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/runtime"
)

// CaptureStore persists capture runs the task runtime hands it, satisfying
// runtime.CaptureRecorder against *db.Pool.
//
// It lives here rather than as a method on *db.Pool because internal/runtime
// already imports internal/db (its kill switch reads account state via
// NewDBKillSwitch), so internal/db implementing runtime.CaptureRecorder
// directly — which requires naming runtime.CaptureFrameRef in a method
// signature, and therefore importing internal/runtime — would close a cycle:
// db -> runtime -> db. internal/db must stay the side that does not import
// internal/runtime, the same discipline the package comment above already
// applies to internal/roster (see internal/db/analytics.go): storage may
// depend on domain logic, but the reverse would make the cycle possible the
// moment either side gained one more import.
//
// internal/ingest sits downstream of both — it already turns stored capture
// frames into facts — and nothing imports it back, so it is where the two
// meet. CaptureStore does the type conversion at that seam and delegates to
// (*db.Pool).RecordCapture, which does the actual transactional write.
//
// Named CaptureStore rather than Store to leave that name for the read+write
// data-access interface Ingester uses below — a task-runtime-facing write
// adapter and an analytics-facing read/write surface are different
// consumers, and giving them the same name in the same package invited a
// collision the moment the second one was written.
type CaptureStore struct {
	pool *db.Pool
}

// NewCaptureStore wraps a pool as a runtime.CaptureRecorder.
func NewCaptureStore(pool *db.Pool) *CaptureStore {
	return &CaptureStore{pool: pool}
}

// RecordCapture implements runtime.CaptureRecorder.
func (s *CaptureStore) RecordCapture(ctx context.Context, accountID int64, route string, frames []runtime.CaptureFrameRef, complete bool) error {
	in := make([]db.CaptureFrameInput, len(frames))
	for i, f := range frames {
		in[i] = db.CaptureFrameInput{
			ScreenshotID: f.ScreenshotID,
			Seq:          f.Seq,
			OffsetPx:     f.OffsetPx,
			GroupKey:     f.GroupKey,
		}
	}
	if err := s.pool.RecordCapture(ctx, accountID, route, in, complete); err != nil {
		return fmt.Errorf("ingest: recording %s capture for account %d: %w", route, accountID, err)
	}
	return nil
}
