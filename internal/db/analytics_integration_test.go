//go:build integration

package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// AllianceByTagName is the lookup `control alliance set` needs and
// CurrentAlliance cannot provide: UpsertAlliance's ON CONFLICT target is
// (tag, name), not "whichever row was most recently observed", and those
// two diverge the moment a second alliance has ever been recorded. See
// cmd/control/alliance.go's runAllianceSet for the bug this fixes.
func TestAllianceByTagNameFindsTheMatchingRow(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	tag, name := "ABT-"+suffix, "By Tag Name "+suffix
	id, err := pool.UpsertAlliance(ctx, Alliance{Tag: tag, Name: name, Server: "1380", MemberCount: 97})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}

	got, err := pool.AllianceByTagName(ctx, tag, name)
	if err != nil {
		t.Fatalf("AllianceByTagName: %v", err)
	}
	if got.ID != id || got.Tag != tag || got.Name != name || got.Server != "1380" || got.MemberCount != 97 {
		t.Fatalf("AllianceByTagName(%s, %s) = %+v, want id=%d server=1380 member_count=97", tag, name, got, id)
	}
}

func TestAllianceByTagNameReturnsErrNotFoundForAnUnknownPair(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	if _, err := pool.AllianceByTagName(ctx, "NOPE-"+suffix, "Nothing Here "+suffix); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AllianceByTagName: got %v, want ErrNotFound", err)
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

// SetAllianceMemberCount is the roster route's write-back of a fresh
// "Members: 96/100" read (see internal/ingest/roster.go). Unlike
// UpsertAlliance it updates by id rather than by (tag, name), since by the
// time ingest calls it the alliances row is already known to exist.
func TestSetAllianceMemberCountUpdatesTheExistingRow(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	allianceID, err := pool.UpsertAlliance(ctx, Alliance{Tag: "SMC-" + suffix, Name: "Set Count " + suffix, MemberCount: 1})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}

	if err := pool.SetAllianceMemberCount(ctx, allianceID, 42); err != nil {
		t.Fatalf("SetAllianceMemberCount: %v", err)
	}

	var got int
	if err := pool.QueryRow(ctx, `SELECT member_count FROM alliances WHERE id = $1`, allianceID).Scan(&got); err != nil {
		t.Fatalf("reading back member_count: %v", err)
	}
	if got != 42 {
		t.Errorf("member_count = %d, want 42", got)
	}
}

func TestSetAllianceMemberCountOnUnknownAllianceReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	// No alliance with this id will ever exist: ids are a bigserial
	// starting at 1, so negative is never a real row to collide with.
	if err := pool.SetAllianceMemberCount(ctx, -1, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
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

// CurrentAllianceID is ingest's entry point into ListMembers/MemberAliases:
// this bot tracks exactly one alliance, so "most recently observed" must
// resolve to whichever alliance was most recently ingested, not to
// insertion order or id order.
func TestCurrentAllianceIDReturnsTheMostRecentlyObserved(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	first, err := pool.UpsertAlliance(ctx, Alliance{Tag: "F1-" + suffix, Name: "First " + suffix, MemberCount: 10})
	if err != nil {
		t.Fatalf("UpsertAlliance first: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // guarantee a strictly later observed_at
	second, err := pool.UpsertAlliance(ctx, Alliance{Tag: "S1-" + suffix, Name: "Second " + suffix, MemberCount: 20})
	if err != nil {
		t.Fatalf("UpsertAlliance second: %v", err)
	}

	got, err := pool.CurrentAllianceID(ctx)
	if err != nil {
		t.Fatalf("CurrentAllianceID: %v", err)
	}
	if got != second {
		t.Fatalf("CurrentAllianceID = %d, want %d (first=%d is older)", got, second, first)
	}
}

// CurrentAlliance is CurrentAllianceID's full-row sibling: `control alliance
// show` needs tag, name, server and member_count back, not just an id to
// look up elsewhere.
func TestCurrentAllianceReturnsTheMostRecentlyObservedRow(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	if _, err := pool.UpsertAlliance(ctx, Alliance{Tag: "CA1-" + suffix, Name: "First " + suffix, MemberCount: 10}); err != nil {
		t.Fatalf("UpsertAlliance first: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // guarantee a strictly later observed_at
	second, err := pool.UpsertAlliance(ctx, Alliance{Tag: "CA2-" + suffix, Name: "Second " + suffix, Server: "1380", MemberCount: 20})
	if err != nil {
		t.Fatalf("UpsertAlliance second: %v", err)
	}

	got, err := pool.CurrentAlliance(ctx)
	if err != nil {
		t.Fatalf("CurrentAlliance: %v", err)
	}
	if got.ID != second || got.Tag != "CA2-"+suffix || got.Name != "Second "+suffix || got.Server != "1380" || got.MemberCount != 20 {
		t.Fatalf("CurrentAlliance = %+v, want the second (more recently observed) row", got)
	}
	if got.ObservedAt.IsZero() {
		t.Error("CurrentAlliance.ObservedAt is zero, want the alliances.observed_at value")
	}
}

// CurrentAlliance's ErrNotFound path is the whole reason task 19 exists: a
// fresh deployment's `alliances` table has no rows at all, and `control
// ingest` needs that condition to surface as a clear instruction rather than
// a bare "not found".
//
// This cannot be observed against the shared testPool database: it is never
// dropped between `make test-integration` runs (see every other test's own
// cleanup comments in this file), so by the time this suite has run more
// than once, `alliances` reliably holds rows from the tests above. Wiping it
// with an unscoped DELETE was considered and rejected — `go test ./...`
// starts package binaries concurrently (internal/dbtest's own migration-lock
// comment exists because of exactly that), and an unscoped DELETE against a
// table every other integration test in this file also touches is the kind
// of shared-state hazard this project's per-test-suffix cleanup convention
// exists to avoid. So this test provisions its own private, freshly migrated
// database instead, via the same dbtest.Prepare every other test in this
// package uses, just pointed at a name nothing else will ever touch.
func TestCurrentAllianceReturnsErrNotFoundOnEmptyTable(t *testing.T) {
	ctx := context.Background()
	pool := freshAllianceDB(ctx, t)

	if _, err := pool.CurrentAlliance(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CurrentAlliance on an empty table: got %v, want ErrNotFound", err)
	}
}

// freshAllianceDB provisions a database this test alone created and
// migrated, rather than reusing testPool's shared one — see
// TestCurrentAllianceReturnsErrNotFoundOnEmptyTable for why. It reuses
// dbtest.Prepare (creation + advisory-locked migration) by pointing
// LW_TEST_DATABASE_URL at a name unique to this call; t.Setenv scopes that
// override to this test and restores it on return, which is safe here
// specifically because nothing in this package calls t.Parallel().
func freshAllianceDB(ctx context.Context, t *testing.T) *Pool {
	t.Helper()

	base := os.Getenv("LW_TEST_DATABASE_URL")
	if base == "" {
		base = dbtest.DefaultURL
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parsing LW_TEST_DATABASE_URL: %v", err)
	}
	name := fmt.Sprintf("lw_manager_alliance_%d_test", time.Now().UnixNano())
	u.Path = "/" + name
	t.Setenv("LW_TEST_DATABASE_URL", u.String())

	dbURL, err := dbtest.Prepare(ctx, Migrate)
	if err != nil {
		t.Fatalf("dbtest.Prepare(%s): %v", u.String(), err)
	}
	pool, err := Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("Connect(%s): %v", dbURL, err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropDatabase(ctx, t, base, name)
	})
	return pool
}

// dropDatabase best-effort tears down the database freshAllianceDB created,
// so a `make test-integration` run does not leave a fresh, almost-empty
// database behind on the shared Postgres instance every time this test runs.
// It is not load-bearing for correctness — a leaked database costs disk, not
// a wrong test result — so a failure here is logged rather than failing the
// test that already got its assertion.
func dropDatabase(ctx context.Context, t *testing.T, baseURL, name string) {
	t.Helper()
	adminURL, err := url.Parse(baseURL)
	if err != nil {
		t.Logf("dropDatabase(%s): parsing admin url: %v", name, err)
		return
	}
	adminURL.Path = "/postgres"

	conn, err := pgx.Connect(ctx, adminURL.String())
	if err != nil {
		t.Logf("dropDatabase(%s): connecting to maintenance database: %v", name, err)
		return
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Logf("dropDatabase(%s): %v", name, err)
	}
}

// Capture is how ingest (Task 12's VS route) would read a capture's own
// status back; roster ingest does not call it, but it is part of the shared
// Store interface and owes its own coverage rather than riding on that.
func TestCaptureReadsBackOneRun(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	id, err := pool.CreateCapture(ctx, Capture{AccountID: accountID, Route: "roster", ExpectedRows: 96})
	if err != nil {
		t.Fatalf("CreateCapture: %v", err)
	}
	if err := pool.FinishCapture(ctx, id, "complete", 94, ""); err != nil {
		t.Fatalf("FinishCapture: %v", err)
	}

	got, err := pool.Capture(ctx, id)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got.ID != id || got.AccountID != accountID || got.Route != "roster" || got.Status != "complete" ||
		got.ExpectedRows != 96 || got.ParsedRows != 94 {
		t.Fatalf("Capture = %+v, want id=%d account=%d route=roster status=complete expected=96 parsed=94",
			got, id, accountID)
	}
	if got.StartedAt.IsZero() {
		t.Error("Capture.StartedAt is zero, want the captures.started_at default")
	}
}

func TestCaptureUnknownIDReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	if _, err := pool.Capture(ctx, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Capture(-1): got %v, want ErrNotFound", err)
	}
}

// ScreenshotObjectKey is how ingest turns a capture_frames.screenshot_id
// into the blob store key it fetches pixels from — the seam this task adds
// between a stored capture and the bytes ingest actually reads.
func TestScreenshotObjectKeyResolvesTheStoredKey(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	shotID := dbtest.SeedScreenshot(ctx, t, pool, accountID)

	key, err := pool.ScreenshotObjectKey(ctx, shotID)
	if err != nil {
		t.Fatalf("ScreenshotObjectKey: %v", err)
	}
	if key != "test/key.png" {
		t.Fatalf("key = %q, want %q", key, "test/key.png")
	}
}

func TestScreenshotObjectKeyUnknownIDReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	if _, err := pool.ScreenshotObjectKey(ctx, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ScreenshotObjectKey(-1): got %v, want ErrNotFound", err)
	}
}

// MemberAliases must scope to the alliance and to active members the same
// way ListMembers does — the two are folded together into roster.Member at
// the ingest seam, and a drift between their scopes would let a stale or
// foreign alias silently match a row it should not.
func TestMemberAliasesScopesToAllianceAndActive(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	a1, err := pool.UpsertAlliance(ctx, Alliance{Tag: "MA1-" + suffix, Name: "Alpha " + suffix, MemberCount: 2})
	if err != nil {
		t.Fatalf("UpsertAlliance a1: %v", err)
	}
	a2, err := pool.UpsertAlliance(ctx, Alliance{Tag: "MA2-" + suffix, Name: "Bravo " + suffix, MemberCount: 1})
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
	dave, err := pool.CreateMember(ctx, Member{AllianceID: a2, Name: "Dave", NameNormalized: "dave"})
	if err != nil {
		t.Fatalf("CreateMember Dave: %v", err)
	}

	for _, al := range []struct {
		memberID int64
		alias    string
	}{
		{alice, "Alicia"}, {alice, "AL1c3"}, {bob, "Bobby"}, {carol, "Carrie"}, {dave, "David"},
	} {
		if err := pool.AddAlias(ctx, al.memberID, al.alias, "manual"); err != nil {
			t.Fatalf("AddAlias(%d, %q): %v", al.memberID, al.alias, err)
		}
	}

	got, err := pool.MemberAliases(ctx, a1)
	if err != nil {
		t.Fatalf("MemberAliases: %v", err)
	}
	if _, ok := got[carol]; ok {
		t.Errorf("MemberAliases(a1) includes soft-deleted Carol: %+v", got)
	}
	if _, ok := got[dave]; ok {
		t.Errorf("MemberAliases(a1) includes Dave from a2: %+v", got)
	}
	aliceAliases := append([]string(nil), got[alice]...)
	sort.Strings(aliceAliases)
	if want := []string{"AL1c3", "Alicia"}; !reflect.DeepEqual(aliceAliases, want) {
		t.Errorf("MemberAliases(a1)[alice] = %+v, want %+v", aliceAliases, want)
	}
	if want := []string{"Bobby"}; !reflect.DeepEqual(got[bob], want) {
		t.Errorf("MemberAliases(a1)[bob] = %+v, want %+v", got[bob], want)
	}
}

// seedPendingReview queues one review item with a unique raw_text (so a
// database that is never dropped between `make test-integration` runs can
// never confuse this run's row with a previous one) and returns its id.
func seedPendingReview(ctx context.Context, t *testing.T, pool *Pool, accountID int64, candidates []roster.Candidate) (id, captureID int64, rawText string) {
	t.Helper()
	captureID, err := pool.CreateCapture(ctx, Capture{AccountID: accountID, Route: "vs_ranking"})
	if err != nil {
		t.Fatalf("CreateCapture: %v", err)
	}
	shotID := dbtest.SeedScreenshot(ctx, t, pool, accountID)
	rawText = "kaln445-" + testSuffix()
	id, err = pool.QueueReview(ctx, ReviewItem{
		CaptureID: captureID, ScreenshotID: shotID, RowY0: 100, RowY1: 140,
		RawText: rawText, Candidates: candidates, Reason: "ambiguous_name_match",
	})
	if err != nil {
		t.Fatalf("QueueReview: %v", err)
	}
	return id, captureID, rawText
}

func TestPendingReviewsListsAQueuedItemAndReviewFetchesItByID(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	candidates := []roster.Candidate{{MemberID: 1, Name: "Kain445", Score: 86}}
	id, _, rawText := seedPendingReview(ctx, t, pool, accountID, candidates)

	pending, err := pool.PendingReviews(ctx)
	if err != nil {
		t.Fatalf("PendingReviews: %v", err)
	}
	var found *ReviewItem
	for i := range pending {
		if pending[i].ID == id {
			found = &pending[i]
		}
	}
	if found == nil {
		t.Fatalf("PendingReviews did not include review %d among %+v", id, pending)
	}
	if found.RawText != rawText {
		t.Errorf("RawText = %q, want %q", found.RawText, rawText)
	}
	if !reflect.DeepEqual(found.Candidates, candidates) {
		t.Errorf("Candidates = %+v, want %+v", found.Candidates, candidates)
	}

	got, err := pool.Review(ctx, id)
	if err != nil {
		t.Fatalf("Review(%d): %v", id, err)
	}
	if got.RawText != rawText || !reflect.DeepEqual(got.Candidates, candidates) {
		t.Fatalf("Review(%d) = %+v, want raw_text %q and candidates %+v", id, got, rawText, candidates)
	}
}

func TestReviewUnknownIDReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	if _, err := pool.Review(ctx, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Review(-1): err = %v, want ErrNotFound", err)
	}
}

// ResolveReview must write the alias, so tomorrow's identical misread
// matches directly instead of queueing again -- and, having done that, must
// not let a second resolve of the same item (a duplicate form submit) write
// a second alias or resolve something that is no longer pending.
func TestResolveReviewWritesAnAliasAndIsNotDoubleApplied(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	allianceID, err := pool.UpsertAlliance(ctx, Alliance{Tag: "RVW" + testSuffix(), Name: "Review Test Alliance"})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}
	// The member name is unique to this test run, not the literal "Kain445"
	// TestFactsAreAppendOnlyAndSupersede uses: that test's own cleanup step
	// (`DELETE FROM members WHERE name = 'Kain445'`) would otherwise hit the
	// member_aliases row ResolveReview writes below on a later run, since
	// member_aliases has no ON DELETE CASCADE from members -- turning this
	// test's leftover state into a foreign-key failure in an unrelated test.
	memberName := "Kain445-" + testSuffix()
	memberID, err := pool.CreateMember(ctx, Member{
		AllianceID: allianceID, Name: memberName, NameNormalized: roster.Normalize(memberName),
	})
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}
	accountID := dbtest.SeedAccount(ctx, t, pool)
	id, captureID, rawText := seedPendingReview(ctx, t, pool, accountID, []roster.Candidate{{MemberID: memberID, Name: memberName, Score: 86}})

	// The returned item is what handleReviewResolve uses to tell a reviewer
	// which capture to re-ingest, per the project owner's ruling: resolving
	// writes the alias only, and the fact arrives by re-running ingest over
	// this same capture.
	resolved, err := pool.ResolveReview(ctx, id, memberID, "reviewer@test")
	if err != nil {
		t.Fatalf("ResolveReview: %v", err)
	}
	if resolved.CaptureID != captureID {
		t.Errorf("ResolveReview CaptureID = %d, want %d", resolved.CaptureID, captureID)
	}

	aliases, err := pool.MemberAliases(ctx, allianceID)
	if err != nil {
		t.Fatalf("MemberAliases: %v", err)
	}
	if got := aliases[memberID]; len(got) != 1 || got[0] != rawText {
		t.Fatalf("MemberAliases[%d] = %+v, want [%q]", memberID, got, rawText)
	}

	pending, err := pool.PendingReviews(ctx)
	if err != nil {
		t.Fatalf("PendingReviews: %v", err)
	}
	for _, p := range pending {
		if p.ID == id {
			t.Fatalf("review %d is still pending after being resolved", id)
		}
	}

	// The guard the "WHERE status = 'pending'" clause exists for: resolving
	// the same item again (a duplicate form submit) must not write a second
	// alias, and must report ErrNotFound rather than silently succeeding.
	if _, err := pool.ResolveReview(ctx, id, memberID, "reviewer@test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second ResolveReview: err = %v, want ErrNotFound", err)
	}
	aliases, err = pool.MemberAliases(ctx, allianceID)
	if err != nil {
		t.Fatalf("MemberAliases (after second resolve attempt): %v", err)
	}
	if got := aliases[memberID]; len(got) != 1 {
		t.Fatalf("MemberAliases[%d] = %+v after a duplicate resolve, want still exactly 1", memberID, got)
	}
}

// Per the project owner's ruling on the task-14 defect report: a fact
// arrives by re-running ingest over the review item's capture, never by
// resolving the review item itself. Storing the row's already-OCR'd value on
// review_queue so a resolve could write the fact directly was considered and
// rejected -- it would be a second copy of the number living outside
// participation_facts, and the fact would be built from that copy rather
// than re-derived from the pixels, weaker provenance than every other fact
// in the system. Verified here directly rather than left as a documented
// assumption.
func TestResolveReviewWritesNoParticipationFact(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	allianceID, err := pool.UpsertAlliance(ctx, Alliance{Tag: "RVF" + testSuffix(), Name: "Review Fact Test Alliance"})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}
	// See TestResolveReviewWritesAnAliasAndIsNotDoubleApplied for why this
	// member's name must not literally be "Kain445".
	memberName := "Kain445-" + testSuffix()
	memberID, err := pool.CreateMember(ctx, Member{
		AllianceID: allianceID, Name: memberName, NameNormalized: roster.Normalize(memberName),
	})
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}
	accountID := dbtest.SeedAccount(ctx, t, pool)
	id, _, _ := seedPendingReview(ctx, t, pool, accountID, []roster.Candidate{{MemberID: memberID, Name: memberName, Score: 86}})

	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM participation_facts WHERE member_id = $1`, memberID).Scan(&before); err != nil {
		t.Fatalf("counting facts before resolve: %v", err)
	}
	if _, err := pool.ResolveReview(ctx, id, memberID, "reviewer@test"); err != nil {
		t.Fatalf("ResolveReview: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM participation_facts WHERE member_id = $1`, memberID).Scan(&after); err != nil {
		t.Fatalf("counting facts after resolve: %v", err)
	}
	if after != before {
		t.Fatalf("participation_facts for member %d went from %d to %d rows across a resolve; want unchanged", memberID, before, after)
	}
}

// RejectReview marks the item rejected without writing an alias for anyone
// -- a rejection is a statement that the row taught nothing, not a claim
// about whose row it was.
func TestRejectReviewMarksRejectedAndWritesNoAlias(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	id, _, _ := seedPendingReview(ctx, t, pool, accountID, nil)

	if err := pool.RejectReview(ctx, id, "reviewer@test"); err != nil {
		t.Fatalf("RejectReview: %v", err)
	}

	pending, err := pool.PendingReviews(ctx)
	if err != nil {
		t.Fatalf("PendingReviews: %v", err)
	}
	for _, p := range pending {
		if p.ID == id {
			t.Fatalf("review %d is still pending after being rejected", id)
		}
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM review_queue WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if status != "rejected" {
		t.Fatalf("status = %q, want rejected", status)
	}

	if err := pool.RejectReview(ctx, id, "reviewer@test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second RejectReview: err = %v, want ErrNotFound", err)
	}
}
