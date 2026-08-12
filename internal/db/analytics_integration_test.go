//go:build integration

package db

import (
	"context"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/dbtest"
)

func TestFactsAreAppendOnlyAndSupersede(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	// Clean slate: the test database is never dropped between
	// `make test-integration` invocations (see internal/dbtest), and unlike
	// UpsertAlliance, CreateMember is not idempotent — it mints a fresh row
	// every run. Without this, a second run would leave a stale Kain445 fact
	// in the (vs_points, 2026-W33) live set from the previous run, and
	// LiveFacts — correctly, per its documented (metric, periodKey) scope —
	// would return both members' rows.
	if _, err := pool.Exec(ctx, `DELETE FROM participation_facts WHERE period_key = '2026-W33' AND metric = 'vs_points'`); err != nil {
		t.Fatalf("cleanup facts: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM members WHERE name = 'Kain445'`); err != nil {
		t.Fatalf("cleanup members: %v", err)
	}

	allianceID, err := pool.UpsertAlliance(ctx, Alliance{Tag: "OrCa", Name: "Organized Chaos", MemberCount: 96})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}
	memberID, err := pool.CreateMember(ctx, Member{AllianceID: allianceID, Name: "Kain445", NameNormalized: "kain445", Rank: "R3"})
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}

	obs := time.Now().UTC().Truncate(time.Second)
	first, err := pool.InsertFact(ctx, Fact{
		MemberID: memberID, Metric: "vs_points", Value: 60158133,
		ObservedAt: obs, PeriodKey: "2026-W33", Source: "ocr:vs_ranking", Confidence: 0.94,
	})
	if err != nil {
		t.Fatalf("InsertFact: %v", err)
	}

	// A correction is a new row that supersedes, never an update in place.
	second, err := pool.InsertFact(ctx, Fact{
		MemberID: memberID, Metric: "vs_points", Value: 60158134,
		ObservedAt: obs.Add(time.Second), PeriodKey: "2026-W33", Source: "manual", Confidence: 1.0,
	})
	if err != nil {
		t.Fatalf("InsertFact (correction): %v", err)
	}
	if err := pool.SupersedeFact(ctx, first, second); err != nil {
		t.Fatalf("SupersedeFact: %v", err)
	}

	live, err := pool.LiveFacts(ctx, "vs_points", "2026-W33")
	if err != nil {
		t.Fatalf("LiveFacts: %v", err)
	}
	if len(live) != 1 || live[0].ID != second {
		t.Fatalf("live facts = %+v, want only the superseding row %d", live, second)
	}
}

func TestUpsertAllianceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	a := Alliance{Tag: "OrCa", Name: "Organized Chaos", MemberCount: 96}
	first, err := pool.UpsertAlliance(ctx, a)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	a.MemberCount = 95
	second, err := pool.UpsertAlliance(ctx, a)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("ids differ: %d then %d — upsert must not create a duplicate", first, second)
	}
}

func TestCaptureFramesCascadeWithTheirCapture(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	captureID, err := pool.CreateCapture(ctx, Capture{AccountID: accountID, Route: "vs_ranking", ExpectedRows: 96})
	if err != nil {
		t.Fatalf("CreateCapture: %v", err)
	}
	shotID := dbtest.SeedScreenshot(ctx, t, pool, accountID)
	if err := pool.AddCaptureFrame(ctx, CaptureFrame{CaptureID: captureID, Seq: 0, ScreenshotID: shotID, OffsetPx: 0}); err != nil {
		t.Fatalf("AddCaptureFrame: %v", err)
	}
	if err := pool.FinishCapture(ctx, captureID, "complete", 94, ""); err != nil {
		t.Fatalf("FinishCapture: %v", err)
	}

	frames, err := pool.CaptureFrames(ctx, captureID)
	if err != nil {
		t.Fatalf("CaptureFrames: %v", err)
	}
	if len(frames) != 1 || frames[0].ScreenshotID != shotID {
		t.Fatalf("frames = %+v", frames)
	}
}
