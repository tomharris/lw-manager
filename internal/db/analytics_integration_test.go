//go:build integration

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/dbtest"
	"github.com/tomharris/lw-manager/internal/roster"
)

// testSuffix mints a value unique to this call, so tests that seed rows with
// no natural per-call uniqueness (an alliance tag, say) do not accumulate
// state across repeated `make test-integration` runs against a database that
// is never dropped between invocations.
func testSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

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

// RecordCapture is the transactional writer Task 10 adds: the capture row
// and every frame must commit together, with status derived from complete
// rather than accepted as free text, and frames stored exactly as given —
// not renumbered, not recomputed.
func TestRecordCaptureCommitsCaptureAndFramesTogether(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	shot0 := dbtest.SeedScreenshot(ctx, t, pool, accountID)
	shot1 := dbtest.SeedScreenshot(ctx, t, pool, accountID)

	// Deliberately out of Seq order in the input slice: RecordCapture must
	// not assume or impose per-call ordering, only store what it is given.
	frames := []CaptureFrameInput{
		{ScreenshotID: shot1, Seq: 1, OffsetPx: 512, GroupKey: "R3"},
		{ScreenshotID: shot0, Seq: 0, OffsetPx: 0, GroupKey: "R3"},
	}
	if err := pool.RecordCapture(ctx, accountID, "roster", frames, true); err != nil {
		t.Fatalf("RecordCapture: %v", err)
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
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2: %+v", len(got), got)
	}
	// CaptureFrames orders by seq, so index 0 must be the seq-0 frame
	// regardless of the order RecordCapture received them in.
	if got[0].Seq != 0 || got[0].ScreenshotID != shot0 || got[0].OffsetPx != 0 || got[0].GroupKey != "R3" {
		t.Fatalf("frame[0] = %+v, want seq 0 screenshot %d offset 0 group R3", got[0], shot0)
	}
	if got[1].Seq != 1 || got[1].ScreenshotID != shot1 || got[1].OffsetPx != 512 || got[1].GroupKey != "R3" {
		t.Fatalf("frame[1] = %+v, want seq 1 screenshot %d offset 512 group R3", got[1], shot1)
	}
}

// complete=false must map to 'partial', never to 'running' or anything else
// the CHECK constraint would reject — and the distinction is load-bearing:
// ingest reads an absent member as a zero score only on a complete capture.
func TestRecordCaptureIncompleteMapsToPartial(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	shot := dbtest.SeedScreenshot(ctx, t, pool, accountID)

	if err := pool.RecordCapture(ctx, accountID, "vs_ranking",
		[]CaptureFrameInput{{ScreenshotID: shot, Seq: 0}}, false); err != nil {
		t.Fatalf("RecordCapture: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM captures WHERE account_id = $1 AND route = 'vs_ranking'`, accountID,
	).Scan(&status); err != nil {
		t.Fatalf("reading capture row: %v", err)
	}
	if status != "partial" {
		t.Fatalf("status = %q, want %q", status, "partial")
	}
}

// A mid-write failure (here, a duplicate Seq colliding with capture_frames'
// UNIQUE (capture_id, seq) constraint) must roll back the whole write. A
// killed process — or, as here, a rejected write — must never leave the
// capture row committed with some but not all of its frames, or committed
// with none at all while still visible.
func TestRecordCaptureMidWriteFailureLeavesNoOrphanRows(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	shot0 := dbtest.SeedScreenshot(ctx, t, pool, accountID)
	shot1 := dbtest.SeedScreenshot(ctx, t, pool, accountID)

	frames := []CaptureFrameInput{
		{ScreenshotID: shot0, Seq: 0},
		{ScreenshotID: shot1, Seq: 0}, // duplicate Seq: violates the UNIQUE constraint
	}
	if err := pool.RecordCapture(ctx, accountID, "roster", frames, true); err == nil {
		t.Fatal("RecordCapture: want an error from the duplicate Seq, got nil")
	}

	var captureCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM captures WHERE account_id = $1`, accountID,
	).Scan(&captureCount); err != nil {
		t.Fatalf("counting captures: %v", err)
	}
	if captureCount != 0 {
		t.Fatalf("captures for account %d = %d, want 0 — the failed write must not leave a capture row behind", accountID, captureCount)
	}

	var frameCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM capture_frames cf JOIN captures c ON c.id = cf.capture_id WHERE c.account_id = $1`, accountID,
	).Scan(&frameCount); err != nil {
		t.Fatalf("counting frames: %v", err)
	}
	if frameCount != 0 {
		t.Fatalf("capture_frames for account %d = %d, want 0", accountID, frameCount)
	}
}

// AddAlias must store exactly the form roster.Normalize would compute, not
// some independent lowercase-only approximation: if the two drift apart,
// alias lookup at match time silently stops finding rows a human already
// confirmed, and the review queue keeps re-asking about names that were
// already resolved.
func TestAddAliasStoresTheFormMatchingLooksUp(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	allianceID, err := pool.UpsertAlliance(ctx, Alliance{Tag: "AL-" + suffix, Name: "Alliance " + suffix, MemberCount: 1})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}
	memberID, err := pool.CreateMember(ctx, Member{
		AllianceID: allianceID, Name: "Michell", NameNormalized: roster.Normalize("Michell"),
	})
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}

	// Letter-spaced OCR read is the real observed case documented in
	// roster.TokenSetRatio; it is exactly what a human confirms via AddAlias
	// after the review queue flags it.
	const decorated = "M I C H E L L"
	if err := pool.AddAlias(ctx, memberID, decorated, "manual"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}

	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT alias_normalized FROM member_aliases WHERE member_id = $1`, memberID,
	).Scan(&stored); err != nil {
		t.Fatalf("reading alias_normalized: %v", err)
	}

	want := roster.Normalize(decorated)
	if stored != want {
		t.Fatalf("alias_normalized = %q, want %q (roster.Normalize(%q), what a lookup would compute)", stored, want, decorated)
	}
}

// The dedup guarantee AddAlias exists to provide: repeating the same alias,
// or a differently-decorated alias that normalizes to the same value, must
// not multiply rows. The second case is the one the (member_id,
// alias_normalized) conflict target specifically exists to catch — a naive
// conflict on the raw alias column would let it through.
func TestAddAliasIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	allianceID, err := pool.UpsertAlliance(ctx, Alliance{Tag: "AL-" + suffix, Name: "Alliance " + suffix, MemberCount: 1})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}
	memberID, err := pool.CreateMember(ctx, Member{
		AllianceID: allianceID, Name: "Michell", NameNormalized: roster.Normalize("Michell"),
	})
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}

	if err := pool.AddAlias(ctx, memberID, "Michell", "manual"); err != nil {
		t.Fatalf("AddAlias (first): %v", err)
	}
	if err := pool.AddAlias(ctx, memberID, "Michell", "manual"); err != nil {
		t.Fatalf("AddAlias (exact repeat): %v", err)
	}
	if err := pool.AddAlias(ctx, memberID, "M I C H E L L", "manual"); err != nil {
		t.Fatalf("AddAlias (differently decorated, same normalized form): %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM member_aliases WHERE member_id = $1`, memberID,
	).Scan(&count); err != nil {
		t.Fatalf("counting aliases: %v", err)
	}
	if count != 1 {
		t.Fatalf("alias count = %d, want 1 — the conflict target is (member_id, alias_normalized)", count)
	}
}

// ListMembers must scope strictly to the requested alliance and to active
// members: a member from another alliance, or a soft-deleted member in this
// one, must never leak into the roster route's view.
func TestListMembersScopesToAllianceAndActive(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	a1, err := pool.UpsertAlliance(ctx, Alliance{Tag: "A1-" + suffix, Name: "Alpha " + suffix, MemberCount: 2})
	if err != nil {
		t.Fatalf("UpsertAlliance a1: %v", err)
	}
	a2, err := pool.UpsertAlliance(ctx, Alliance{Tag: "A2-" + suffix, Name: "Bravo " + suffix, MemberCount: 1})
	if err != nil {
		t.Fatalf("UpsertAlliance a2: %v", err)
	}

	alice, err := pool.CreateMember(ctx, Member{AllianceID: a1, Name: "Alice", NameNormalized: "alice"})
	if err != nil {
		t.Fatalf("CreateMember Alice: %v", err)
	}
	bob, err := pool.CreateMember(ctx, Member{AllianceID: a1, Name: "Bob", NameNormalized: "bob"})
	if err != nil {
		t.Fatalf("CreateMember Bob: %v", err)
	}
	carol, err := pool.CreateMember(ctx, Member{AllianceID: a1, Name: "Carol", NameNormalized: "carol"})
	if err != nil {
		t.Fatalf("CreateMember Carol: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE members SET active = false, left_at = now() WHERE id = $1`, carol,
	); err != nil {
		t.Fatalf("soft-deleting Carol: %v", err)
	}
	if _, err := pool.CreateMember(ctx, Member{AllianceID: a2, Name: "Dave", NameNormalized: "dave"}); err != nil {
		t.Fatalf("CreateMember Dave: %v", err)
	}

	members, err := pool.ListMembers(ctx, a1)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}

	got := make(map[int64]bool, len(members))
	for _, m := range members {
		got[m.ID] = true
	}
	if len(members) != 2 || !got[alice] || !got[bob] || got[carol] {
		t.Fatalf("ListMembers(a1) = %+v, want exactly Alice and Bob (active, in a1) — not Carol (soft-deleted) or Dave (a2)", members)
	}
}

// QueueReview's candidates_json is how a human sees the ranked options; a
// silent encoding mismatch would mean the review UI shows nothing or the
// wrong shape for every queued row.
func TestQueueReviewRoundTripsCandidates(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	captureID, err := pool.CreateCapture(ctx, Capture{AccountID: accountID, Route: "vs_ranking", ExpectedRows: 10})
	if err != nil {
		t.Fatalf("CreateCapture: %v", err)
	}
	shotID := dbtest.SeedScreenshot(ctx, t, pool, accountID)

	candidates := []roster.Candidate{
		{MemberID: 1, Name: "Michell", Score: 88},
		{MemberID: 2, Name: "Michelle", Score: 81},
	}
	id, err := pool.QueueReview(ctx, ReviewItem{
		CaptureID: captureID, ScreenshotID: shotID,
		RowY0: 100, RowY1: 140, RawText: "M1CHELL",
		Candidates: candidates, Reason: "below auto-accept threshold",
	})
	if err != nil {
		t.Fatalf("QueueReview: %v", err)
	}

	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT candidates_json FROM review_queue WHERE id = $1`, id,
	).Scan(&raw); err != nil {
		t.Fatalf("reading candidates_json: %v", err)
	}
	var got []roster.Candidate
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshalling candidates_json %s: %v", raw, err)
	}
	if !reflect.DeepEqual(got, candidates) {
		t.Fatalf("candidates_json round-trip = %+v, want %+v", got, candidates)
	}
}
