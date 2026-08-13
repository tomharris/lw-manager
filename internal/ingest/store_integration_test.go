//go:build integration

package ingest

import (
	"context"
	"testing"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/dbtest"
	"github.com/tomharris/lw-manager/internal/runtime"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	ctx := context.Background()
	url, err := dbtest.Prepare(ctx, db.Migrate)
	if err != nil {
		t.Fatalf("dbtest.Prepare(): %v", err)
	}
	p, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Store is the seam between runtime.CaptureRecorder and *db.Pool: this
// exercises it end to end, through runtime.CaptureFrameRef, the type task
// code actually builds, rather than through db.CaptureFrameInput directly.
func TestStoreRecordCaptureRoundTripsThroughRuntimeTypes(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := NewStore(pool)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	shot := dbtest.SeedScreenshot(ctx, t, pool, accountID)

	frames := []runtime.CaptureFrameRef{
		{ScreenshotID: shot, Seq: 0, OffsetPx: 640, GroupKey: "R4"},
	}
	if err := store.RecordCapture(ctx, accountID, "roster", frames, true); err != nil {
		t.Fatalf("Store.RecordCapture: %v", err)
	}

	var status string
	var captureID int64
	if err := pool.QueryRow(ctx,
		`SELECT id, status FROM captures WHERE account_id = $1 AND route = 'roster'`, accountID,
	).Scan(&captureID, &status); err != nil {
		t.Fatalf("reading capture row: %v", err)
	}
	if status != "complete" {
		t.Fatalf("status = %q, want %q", status, "complete")
	}

	got, err := pool.CaptureFrames(ctx, captureID)
	if err != nil {
		t.Fatalf("CaptureFrames: %v", err)
	}
	if len(got) != 1 || got[0].OffsetPx != 640 || got[0].GroupKey != "R4" {
		t.Fatalf("frames = %+v, want one frame offset 640 group R4", got)
	}
}
