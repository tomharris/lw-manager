//go:build integration

package ingest

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"testing"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/dbtest"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
)

// vsPoolFixture is the VS route wired against a real Postgres store and fake
// everything else. The pixels and the OCR reads are fakes because they are not
// what is under test here; the store is real because participation_facts'
// UNIQUE (member_id, metric, period_key, source, observed_at) is the thing
// under test, and package-local fakes do not enforce constraints. That gap is
// exactly why the whole VS route could be re-run-unsafe with every unit test
// passing.
type vsPoolFixture struct {
	pool      *db.Pool
	ingester  *Ingester
	captureID int64
	memberIDs map[string]int64
}

// newVSPoolFixture seeds one complete vs_ranking capture: an alliance, the
// named members, one synthetic frame carrying a row per name in rankedNames,
// and the scripted OCR reads for it. runs says how many IngestVS calls the
// test will make — ocr.FakeEngine replays a finite script and errors once it
// is exhausted, so a test that ingests twice must script the same reads twice.
func newVSPoolFixture(t *testing.T, memberNames, rankedNames []string, runs int) *vsPoolFixture {
	t.Helper()
	ctx := context.Background()
	pool := testPool(t)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	allianceID, err := pool.UpsertAlliance(ctx, db.Alliance{
		Tag:  fmt.Sprintf("VSI-%d", accountID),
		Name: fmt.Sprintf("VS Integration %d", accountID),
	})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}

	memberIDs := make(map[string]int64, len(memberNames))
	for _, name := range memberNames {
		id, err := pool.CreateMember(ctx, db.Member{
			AllianceID: allianceID, Name: name, NameNormalized: roster.Normalize(name),
		})
		if err != nil {
			t.Fatalf("CreateMember %q: %v", name, err)
		}
		memberIDs[name] = id
	}

	blobs := newFakeBlobs()
	key := fmt.Sprintf("vs-integration-frame-%d", accountID)
	var buf bytes.Buffer
	if err := png.Encode(&buf, vsFrame(len(rankedNames))); err != nil {
		t.Fatalf("encoding synthetic frame: %v", err)
	}
	blobs.objects[key] = buf.Bytes()

	var shotID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO screenshots (account_id, captured_at, object_key, sha256)
		 VALUES ($1, now(), $2, $3) RETURNING id`,
		accountID, key, fmt.Sprintf("%064d", accountID)).Scan(&shotID); err != nil {
		t.Fatalf("seeding screenshot: %v", err)
	}

	if err := pool.RecordCapture(ctx, accountID, "vs_ranking",
		[]db.CaptureFrameInput{{ScreenshotID: shotID, Seq: 0, OffsetPx: 0}}, true); err != nil {
		t.Fatalf("RecordCapture: %v", err)
	}
	var captureID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM captures WHERE account_id = $1 AND route = 'vs_ranking'`, accountID,
	).Scan(&captureID); err != nil {
		t.Fatalf("reading back the capture: %v", err)
	}

	var results []ocr.Result
	for range runs {
		for k, name := range rankedNames {
			results = append(results,
				ocr.Result{Text: name, Confidence: 0.95},
				ocr.Result{Text: commaGroup(90000000 - k*100000), Confidence: 0.95},
			)
		}
	}

	return &vsPoolFixture{
		pool:      pool,
		ingester:  New(pool, blobs, &ocr.FakeEngine{Results: results}),
		captureID: captureID,
		memberIDs: memberIDs,
	}
}

// liveValues returns this fixture's members' live vs_points values, keyed by
// name, so a test can assert on what a leaderboard would actually read.
func (f *vsPoolFixture) liveValues(t *testing.T, periodKey string) map[string]float64 {
	t.Helper()
	facts, err := f.pool.LiveFacts(context.Background(), "vs_points", periodKey)
	if err != nil {
		t.Fatalf("LiveFacts: %v", err)
	}
	byID := map[int64]float64{}
	for _, fact := range facts {
		byID[fact.MemberID] = fact.Value
	}
	out := map[string]float64{}
	for name, id := range f.memberIDs {
		if v, ok := byID[id]; ok {
			out[name] = v
		}
	}
	return out
}

// The resolve-then-reingest loop the studio review page directs an operator to
// run: resolve a queued row, then ingest the same capture again. Every VS fact
// pins observed_at to the capture's own started_at, so a second run recomputes
// the identical (member_id, metric, period_key, source, observed_at) key for
// every member it writes -- both the rows it reads and the zeroes it infers.
// A plain INSERT rejects that outright, which made the loop unusable: the
// second run died on a unique-constraint violation rather than correcting
// anything. This is IngestRoster's task 27 fix applied to the other route.
func TestIngestVSSurvivesARerunOfTheSameCapture(t *testing.T) {
	ctx := context.Background()
	members := []string{"Member01", "Member02", "Member03"}
	fx := newVSPoolFixture(t, members, []string{"Member01", "Member02"}, 2)

	first, err := fx.ingester.IngestVS(ctx, fx.captureID, "2026-W33")
	if err != nil {
		t.Fatalf("first IngestVS: %v", err)
	}

	second, err := fx.ingester.IngestVS(ctx, fx.captureID, "2026-W33")
	if err != nil {
		t.Fatalf("second IngestVS: %v", err)
	}

	// Same pixels, same reads, so the second run must reach the same
	// conclusions rather than merely not crashing.
	if first.Matched != second.Matched || first.Zeroed != second.Zeroed {
		t.Errorf("re-run disagreed with the first: matched %d->%d, zeroed %d->%d",
			first.Matched, second.Matched, first.Zeroed, second.Zeroed)
	}

	// And it must not have doubled anything. Member03 is absent from the
	// ranking and is the inferred zero; the other two are read.
	got := fx.liveValues(t, "2026-W33")
	if len(got) != 3 {
		t.Fatalf("live facts for %d members, want 3: %+v", len(got), got)
	}
	if got["Member03"] != 0 {
		t.Errorf("Member03 = %v, want the inferred zero 0", got["Member03"])
	}
	if got["Member01"] != 90000000 {
		t.Errorf("Member01 = %v, want 90000000", got["Member01"])
	}
}

// The loop the review page exists to serve, end to end: a row whose name OCR
// could not match is queued, a human resolves it to a member (writing an
// alias), and the capture is ingested again so the corrected row becomes a
// fact. Nothing covered this seam before -- studio's own test proves the
// alias is written and db's proves ResolveReview is not double-applied, but
// neither proves the next ingest actually produces the number.
//
// The read deliberately carries 0.85 confidence: above factConfidenceGate
// (0.80) so it is a legitimate fact, and below zeroInferenceConfidence (0.90)
// so it is the case that breaks if an inferred zero was written for this
// member on the first pass. UpsertFact only overwrites on a strictly higher
// confidence, so a stale zero would outrank the correction and silently
// survive it -- a wrong number on a leaderboard, which is worse than the
// crash this route used to produce. The fix is upstream of the collision: a
// capture holding rows it could not attribute has not proved anyone absent,
// so it infers no zeroes at all.
func TestIngestVSReingestAfterAResolvedReviewReplacesTheInferredZero(t *testing.T) {
	ctx := context.Background()
	const garbled = "MemberOI"

	members := []string{"Member01", "Member02"}
	fx := newVSPoolFixture(t, members, []string{garbled, "Member02"}, 2)
	// Re-script so the corrected row's points read below the inferred-zero
	// confidence; newVSPoolFixture's default 0.95 would clear the guard by
	// luck rather than by design and prove nothing.
	fx.ingester.engine = &ocr.FakeEngine{Results: []ocr.Result{
		{Text: garbled, Confidence: 0.95}, {Text: commaGroup(90000000), Confidence: 0.85},
		{Text: "Member02", Confidence: 0.95}, {Text: commaGroup(89900000), Confidence: 0.95},
		{Text: garbled, Confidence: 0.95}, {Text: commaGroup(90000000), Confidence: 0.85},
		{Text: "Member02", Confidence: 0.95}, {Text: commaGroup(89900000), Confidence: 0.95},
	}}

	first, err := fx.ingester.IngestVS(ctx, fx.captureID, "2026-W33")
	if err != nil {
		t.Fatalf("first IngestVS: %v", err)
	}
	if first.Queued == 0 {
		t.Fatal("the unmatched row must have reached review for this test to mean anything")
	}
	// A capture still holding an unattributed row has not proved anyone
	// absent from the ranking.
	if first.Zeroed != 0 {
		t.Errorf("zeroed %d while %d rows were unidentified, want 0", first.Zeroed, first.Queued)
	}

	// What resolving that review in studio does: bind the raw text to the
	// member it really was.
	if err := fx.pool.AddAlias(ctx, fx.memberIDs["Member01"], garbled, "review"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}

	if _, err := fx.ingester.IngestVS(ctx, fx.captureID, "2026-W33"); err != nil {
		t.Fatalf("second IngestVS: %v", err)
	}

	got := fx.liveValues(t, "2026-W33")
	if got["Member01"] != 90000000 {
		t.Errorf("Member01 = %v after the resolve-then-reingest loop, want 90000000; a value of 0 means an inferred zero outranked the correction",
			got["Member01"])
	}
}
