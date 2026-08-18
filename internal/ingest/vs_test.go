package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
)

// --- fakes -----------------------------------------------------------------

// vsFakeStore wraps the roster route's fakeIngestStore and overrides Capture
// so a test can pick the capture's status directly. captures.status is set
// once, at capture time, by whichever route did or did not prove it reached
// the bottom of the list — IngestVS trusts that field rather than
// recomputing it from what got parsed, so the fake needs to be able to say
// what it is.
type vsFakeStore struct {
	*fakeIngestStore
	status string
}

func (s *vsFakeStore) Capture(ctx context.Context, id int64) (db.Capture, error) {
	return db.Capture{ID: id, Status: s.status, StartedAt: s.startedAt}, nil
}

// --- harness -----------------------------------------------------------------

// vsIngestHarness wires an Ingester over a vsFakeStore and the same
// fakeBlobs/ocr.FakeEngine fakes the roster route's tests use.
type vsIngestHarness struct {
	*Ingester
	t                *testing.T
	store            *fakeIngestStore
	blobs            *fakeBlobs
	engine           *ocr.FakeEngine
	nextScreenshotID int64
}

func newVSHarness(t *testing.T, status string) *vsIngestHarness {
	t.Helper()
	store := &fakeIngestStore{
		objectKeys: map[int64]string{},
		aliases:    map[int64][]string{},
		allianceID: 1,
	}
	vstore := &vsFakeStore{fakeIngestStore: store, status: status}
	blobs := newFakeBlobs()
	engine := &ocr.FakeEngine{}
	return &vsIngestHarness{
		Ingester: New(vstore, blobs, engine),
		t:        t,
		store:    store,
		blobs:    blobs,
		engine:   engine,
	}
}

// addFrame PNG-encodes img, files it under a fresh screenshot id in the fake
// blob store, and appends a capture_frames row at offsetPx.
func (h *vsIngestHarness) addFrame(img image.Image, offsetPx int) int64 {
	h.t.Helper()
	h.nextScreenshotID++
	id := h.nextScreenshotID

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		h.t.Fatalf("encoding synthetic frame: %v", err)
	}
	key := fmt.Sprintf("vs-frame-%d", id)
	h.store.objectKeys[id] = key
	h.blobs.objects[key] = buf.Bytes()

	h.store.frames = append(h.store.frames, db.CaptureFrame{
		ID: id, CaptureID: 1, Seq: len(h.store.frames), ScreenshotID: id, OffsetPx: offsetPx,
	})
	return id
}

// vsFrame builds a synthetic ranking frame with nRows cards in vsListRegion
// at vsRowPitch, tall enough that none are clipped by the region's lower
// edge. ocr.FakeEngine never looks at the pixels — only SegmentRows does, so
// this only has to be geometrically right (see rosterFrame's own comment for
// why pixel realism is not this package's job to fake, and for why imgH is
// sized tightly around the drawn content rather than padded with thousands
// of blank pixels a real capture would not have).
func vsFrame(nRows int) image.Image {
	imgH := 400
	regionFrac := vsListRegion.Y2 - vsListRegion.Y1
	for float64(imgH)*regionFrac < float64((nRows+1)*vsRowPitch) {
		imgH += 200
	}
	top := int(vsListRegion.Y1 * float64(imgH))
	return cardFrame(200, imgH, top, vsRowPitch-12, 12, nRows)
}

// commaGroup formats n the way this UI actually renders VS points — comma
// grouped, e.g. 90000000 -> "90,000,000" — rather than the bare digit run
// strconv.Itoa would give. It exists because pointsRe no longer accepts an
// ungrouped run of more than three digits (task 23 fix-round finding C1: an
// ungrouped run is exactly the shape a laundered read takes, so a real
// fixture must not look like one). Every real points value this package's
// measurement ever observed was comma grouped (evidence Finding 3 and task
// 23's own re-audit); a fixture using strconv.Itoa was testing a shape that
// does not occur on real pixels.
func commaGroup(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var groups []string
	for len(s) > 3 {
		groups = append([]string{s[len(s)-3:]}, groups...)
		s = s[:len(s)-3]
	}
	groups = append([]string{s}, groups...)
	out := strings.Join(groups, ",")
	if neg {
		out = "-" + out
	}
	return out
}

// vsFixture describes one IngestVS scenario in terms of the roster and the
// rows a single synthetic frame should carry.
type vsFixture struct {
	// captureComplete sets captures.status to "complete" when true and
	// "partial" otherwise.
	captureComplete bool
	// rosterSize pre-registers N real members, "Member01".."MemberNN".
	rosterSize int
	// rankedRows is how many ranking rows the frame carries. The first
	// min(rankedRows, rosterSize) rows read an exact member name (auto-accept
	// match); any further rows read a name that matches nobody.
	rankedRows int
	// duplicateSelfRow appends one more row after rankedRows, reading
	// "Member01" again — the pinned self row appearing a second time at an
	// unrelated screen position, which is a VS-ranking-specific phenomenon
	// (see CLAUDE.md and the recon findings §2).
	duplicateSelfRow bool
	// ghostRows appends N rows reading a name that matches nobody, on top of
	// rankedRows. It exists because rankedRows alone cannot express "some
	// members absent AND some rows unattributed" — pushing rankedRows past
	// rosterSize to get an unmatched row necessarily matches every member,
	// leaving nobody to be absent. That combination is exactly the state the
	// zero-inference rule turns on.
	ghostRows int
}

func newVSIngestHarness(t *testing.T, fx vsFixture) *vsIngestHarness {
	t.Helper()
	status := "partial"
	if fx.captureComplete {
		status = "complete"
	}
	h := newVSHarness(t, status)

	for k := 1; k <= fx.rosterSize; k++ {
		name := fmt.Sprintf("Member%02d", k)
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}

	type row struct{ name, points string }
	var rows []row

	matched := fx.rankedRows
	if matched > fx.rosterSize {
		matched = fx.rosterSize
	}
	for k := 1; k <= matched; k++ {
		rows = append(rows, row{
			name:   fmt.Sprintf("Member%02d", k),
			points: commaGroup(90000000 - k*100000),
		})
	}
	// Rows beyond the roster size cannot possibly match a real member —
	// used by the "never creates a member" test.
	for k := matched; k < fx.rankedRows; k++ {
		rows = append(rows, row{name: "ZzUnrecognizedGhostRow99", points: commaGroup(1000000)})
	}
	for range fx.ghostRows {
		rows = append(rows, row{name: "ZzUnrecognizedGhostRow99", points: commaGroup(1000000)})
	}
	if fx.duplicateSelfRow {
		rows = append(rows, row{name: "Member01", points: commaGroup(5000000)})
	}

	h.addFrame(vsFrame(len(rows)), 0)

	// 0.95 rather than 0.9: a blended confidence of min(matchNorm, conf) must
	// stay comfortably above zeroInferenceConfidence (0.90) for a read fact,
	// or TestIngestVSInferredZeroCarriesLowerConfidenceThanARead's "a read
	// exceeds an inferred zero" assertion would be testing two equal floats.
	var results []ocr.Result
	for _, r := range rows {
		results = append(results,
			ocr.Result{Text: r.name, Confidence: 0.95},
			ocr.Result{Text: r.name, Confidence: 0.95},
			ocr.Result{Text: r.points, Confidence: 0.95},
		)
	}
	h.engine.Results = results

	return h
}

// reviewReasons counts the queued review rows by reason, mirroring
// rosterIngestHarness's helper. The VS route now has several distinct reasons
// a row can be held for, and the assertions below care which gate a row hit
// rather than only that it was queued.
func (h *vsIngestHarness) reviewReasons() map[string]int {
	out := map[string]int{}
	for _, r := range h.store.Reviews {
		out[r.Reason]++
	}
	return out
}

// factFor returns the fact written for memberID, or ok=false if none was.
//
// Exists so a test can assert a fact EXISTS rather than only that any fact it
// happens to find carries the right value -- a bare count comparison or a
// "for every fact, if it matches this member, check its value" loop is
// vacuous against a bug that drops the row entirely (no fact ever iterated,
// so the value check never runs) while an unrelated bug changes the total
// count to something that still satisfies a `!= N` assertion elsewhere. See
// TestIngestVSNeverSeedsABoundFromARetriedRead's history for a case that
// actually happened: without the never-seed guard, Charlie03's row is the one
// dropped (queued as points_out_of_order via monotonicKnown, not Bravo02's),
// and a loop that only checks matching facts it finds never notices Charlie03
// is missing.
func (h *vsIngestHarness) factFor(memberID int64) (db.Fact, bool) {
	for _, f := range h.store.Facts {
		if f.MemberID == memberID {
			return f, true
		}
	}
	return db.Fact{}, false
}

// --- tests -------------------------------------------------------------------

// The rule the recon forced: the ranking lists scorers only, so an absent
// member scored zero -- but only if the capture is known to have reached the
// bottom. On a partial capture absence and truncation are indistinguishable.
func TestIngestVSWritesZeroesOnlyForACompleteCapture(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{
		captureComplete: true,
		rosterSize:      96,
		rankedRows:      94,
	})

	res, err := h.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Zeroed != 2 {
		t.Errorf("zeroed %d, want 2 — 96 members less 94 ranked", res.Zeroed)
	}
}

// A complete capture still holding a row it could not attribute has not
// proved anyone absent from the ranking: that row belongs to some member, and
// zeroing every unscored member on the strength of it puts a 0.90-confidence
// number on a leaderboard for a read that failed. The zeroes are deferred,
// not lost — clear the review queue, re-ingest, and they get written.
//
// See vs_integration_test.go for the other half of why this matters: against
// a real store the stale zero also outranked the corrected read a resolved
// review produced, so the review loop silently changed nothing.
func TestIngestVSInfersNoZeroesWhileAnyRowIsUnidentified(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{
		captureComplete: true,
		rosterSize:      3,
		rankedRows:      2,
		ghostRows:       1,
	})

	res, err := h.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Unidentified != 1 {
		t.Fatalf("unidentified %d, want 1 — the fixture's ghost row matches nobody", res.Unidentified)
	}
	if res.Zeroed != 0 {
		t.Errorf("zeroed %d while a row was unidentified, want 0", res.Zeroed)
	}
	// Member03 is the absent one. The count alone would not catch a zero
	// written under a different member's id.
	for _, f := range h.store.Facts {
		if f.Value == 0 {
			t.Errorf("an inferred zero was written for member %d while a row was unattributed: %+v", f.MemberID, f)
		}
	}
}

// The complement: with every row attributed, the inference is sound again and
// the absent member is zeroed. Without this, the test above would pass just as
// well against an implementation that never infers a zero at all.
func TestIngestVSInfersZeroesOnceEveryRowIsIdentified(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{
		captureComplete: true,
		rosterSize:      3,
		rankedRows:      2,
	})

	res, err := h.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Unidentified != 0 {
		t.Fatalf("unidentified %d, want 0", res.Unidentified)
	}
	if res.Zeroed != 1 {
		t.Errorf("zeroed %d, want 1 — 3 members less 2 ranked", res.Zeroed)
	}
}

// Finding 2 (task-3-findings-round1.md): the assignedMember build loop that
// feeds zero-inference must key on a.Member -- the MEMBER index -- not on the
// row's own position in the assignments slice. Every other VS fixture reads
// members in roster order (row i is always member i), so the two indexings
// are indistinguishable there and a swap would go unnoticed; this fixture
// reads them out of order on purpose. If the loop were keyed on row index
// instead, row 0 (Member03, member index 2) would mark index 0 "assigned"
// and row 1 (Member01, member index 0) would mark index 1 "assigned" --
// zeroing Member03 despite it having a row, and never zeroing Member02, the
// member genuinely absent from the capture.
func TestIngestVSInfersZeroOnTheRightMemberWhenRowsArriveOutOfRosterOrder(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Member01", "Member02", "Member03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(2), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Member03", Confidence: 0.95}, {Text: "Member03", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		{Text: "Member01", Confidence: 0.95}, {Text: "Member01", Confidence: 0.95}, {Text: "8,000,000", Confidence: 0.95},
	}

	res, err := h.IngestVS(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Zeroed != 1 {
		t.Fatalf("zeroed %d, want 1 — Member02 is the only absent member", res.Zeroed)
	}
	want := h.store.members[1].ID // Member02
	var zeroedID int64
	var sawZero bool
	for _, f := range h.store.Facts {
		if f.Metric == "vs_points" && f.Value == 0 {
			sawZero = true
			zeroedID = f.MemberID
		}
	}
	if !sawZero {
		t.Fatal("no inferred zero was written at all")
	}
	if zeroedID != want {
		t.Errorf("inferred zero landed on member %d, want Member02 (%d): the read rows were Member03 then Member01, out of roster order",
			zeroedID, want)
	}
}

// `control ingest` routes vs_ranking captures through IngestVS the same way
// it routes roster captures through IngestRoster, so a fresh deployment hits
// the identical CurrentAllianceID ErrNotFound dead end here — see
// roster_test.go's TestIngestRosterNamesTheFixWhenNoAllianceHasEverBeenSet
// for the full reasoning; this is that same fix, applied to the other route.
func TestIngestVSNamesTheFixWhenNoAllianceHasEverBeenSet(t *testing.T) {
	h := newVSHarness(t, "complete")
	h.store.currentAllianceErr = fmt.Errorf("db: current alliance: %w", db.ErrNotFound)

	_, err := h.IngestVS(context.Background(), 1, "2026-W33")
	if err == nil {
		t.Fatal("IngestVS: want an error when no alliance has ever been observed, got nil")
	}
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("IngestVS error = %v, want it to still wrap db.ErrNotFound via %%w", err)
	}
	if !strings.Contains(err.Error(), "control alliance set") {
		t.Fatalf("IngestVS error = %q, want it to name `control alliance set` as the fix", err.Error())
	}
}

func TestIngestVSWritesNoZeroesOnAPartialCapture(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{
		captureComplete: false,
		rosterSize:      96,
		rankedRows:      40,
	})

	res, err := h.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Zeroed != 0 {
		t.Fatalf("zeroed %d on a partial capture, want 0 — absence and truncation are indistinguishable there", res.Zeroed)
	}
	if res.Status != "partial" {
		t.Errorf("status = %q, want partial", res.Status)
	}
	// The count alone would not catch a stray zero written under a
	// mislabeled reason; assert its absence directly against every fact the
	// store actually holds.
	for _, f := range h.store.Facts {
		if f.Metric == "vs_points" && f.Value == 0 {
			t.Fatalf("a zero vs_points fact was written on a partial capture: %+v", f)
		}
	}
}

// A VS row that matches nothing must never create a member: the roster
// route is the only writer of that table.
func TestIngestVSNeverCreatesAMember(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{captureComplete: true, rosterSize: 2, rankedRows: 3})

	if _, err := h.IngestVS(context.Background(), 1, "2026-W33"); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if h.store.MembersCreated != 0 {
		t.Errorf("created %d members, want 0", h.store.MembersCreated)
	}
	if len(h.store.Reviews) == 0 {
		t.Error("the unmatched row must have reached review")
	}
}

func TestIngestVSDeduplicatesThePinnedSelfRow(t *testing.T) {
	// The logged-in account appears pinned and in place. Geometric dedupe
	// cannot see that, because the two sit at different screen positions.
	h := newVSIngestHarness(t, vsFixture{
		captureComplete: true, rosterSize: 5, rankedRows: 5, duplicateSelfRow: true,
	})

	res, err := h.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Matched != 5 {
		t.Errorf("matched %d, want 5 — the pinned self row is the same member twice", res.Matched)
	}
	var vsPointsFacts int
	for _, f := range h.store.Facts {
		if f.Metric == "vs_points" {
			vsPointsFacts++
		}
	}
	if vsPointsFacts != 5 {
		t.Errorf("wrote %d vs_points facts, want 5 — the duplicate row must not write a second one", vsPointsFacts)
	}
}

// Requirement 3: an inferred zero is weaker evidence than a read number and
// must carry a lower confidence, so a leaderboard can tell "we saw a zero"
// from "we saw nothing and concluded zero".
func TestIngestVSInferredZeroCarriesLowerConfidenceThanARead(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{captureComplete: true, rosterSize: 3, rankedRows: 2})

	if _, err := h.IngestVS(context.Background(), 1, "2026-W33"); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}

	var sawRead, sawInferredZero bool
	for _, f := range h.store.Facts {
		if f.Metric != "vs_points" {
			continue
		}
		if f.Value == 0 {
			sawInferredZero = true
			if f.Confidence != zeroInferenceConfidence {
				t.Errorf("inferred zero confidence = %v, want the named constant %v", f.Confidence, zeroInferenceConfidence)
			}
			continue
		}
		sawRead = true
		if f.Confidence <= zeroInferenceConfidence {
			t.Errorf("read fact confidence %v does not exceed the inferred-zero confidence %v", f.Confidence, zeroInferenceConfidence)
		}
	}
	if !sawRead || !sawInferredZero {
		t.Fatalf("expected both a read fact and an inferred zero in this fixture, sawRead=%v sawInferredZero=%v", sawRead, sawInferredZero)
	}
}

// A row nothing matches at AutoAccept, whose member is nonetheless the only
// candidate left once every other row is pinned. This is the whole gain: on
// capture 6 it is twelve members, including "Syłar" read as "cular" at 60.
func TestIngestVSResolvesAWeakRowFromTheResidual(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(2), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		// "Br4v0z" scores 68 against Bravo02 (roster.TokenSetRatio) -- inside
		// the 60-79 residual band, well below AutoAccept, and 14 against
		// Alpha01 -- unmatchable per-row, unambiguous once Alpha01 is taken.
		// Landing the raw score inside that band (rather than, say, in the
		// 80s where it would clear factConfidenceGate on score/100 alone) is
		// deliberate: a reviewer proved by mutation that a fixture scoring in
		// the 80s lets this test pass even with the residualMatchConfidence
		// branch deleted, because score/100 alone already clears the gate.
		{Text: "Br4v0z", Confidence: 0.95}, {Text: "Br4v0z", Confidence: 0.95}, {Text: "8,000,000", Confidence: 0.95},
	}

	res, err := h.IngestVS(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Matched != 2 {
		t.Errorf("matched %d, want 2: the residual row's member was the only one left", res.Matched)
	}
	if len(h.store.Facts) != 2 {
		t.Fatalf("wrote %d facts, want 2", len(h.store.Facts))
	}
	// The residual match must carry residualMatchConfidence, not score/100 --
	// a string score of ~60 blended into the fact would fall under
	// factConfidenceGate and the row would be queued anyway, silently undoing
	// the entire assignment.
	var residual db.Fact
	for _, f := range h.store.Facts {
		if f.MemberID == h.store.members[1].ID {
			residual = f
		}
	}
	if residual.Confidence < factConfidenceGate {
		t.Errorf("residual fact confidence %.2f is below factConfidenceGate %.2f; it would never be written",
			residual.Confidence, factConfidenceGate)
	}
	if residual.Confidence >= 0.95 {
		t.Errorf("residual fact confidence %.2f, want it visibly below a clean match", residual.Confidence)
	}
	// The outright assertion: a residual match must carry EXACTLY
	// residualMatchConfidence, not some blend that happens to also clear the
	// two bounds above. Deleting the `matchNorm = residualMatchConfidence`
	// branch in attributeRow must fail this line even if the fixture's raw
	// string score had landed somewhere the bounds above would not have
	// caught.
	if residual.Confidence != residualMatchConfidence {
		t.Errorf("residual fact confidence = %.2f, want exactly residualMatchConfidence (%.2f)",
			residual.Confidence, residualMatchConfidence)
	}
}

// The pinned self row appears twice by design (recon section 2). It is
// structurally expected, so it is counted and dropped -- never queued. A
// review row every week for something the screen always does trains an
// operator to ignore the queue.
func TestIngestVSCountsADuplicateRowRatherThanQueueingIt(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{
		captureComplete: true, rosterSize: 3, rankedRows: 3, duplicateSelfRow: true,
	})

	res, err := h.IngestVS(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Duplicates != 1 {
		t.Errorf("duplicates %d, want 1", res.Duplicates)
	}
	if reasons := h.reviewReasons(); reasons["no_confident_match"] != 0 {
		t.Errorf("the duplicate self row reached the review queue; all reasons: %v", reasons)
	}
	if res.Unidentified != 0 {
		t.Errorf("unidentified %d, want 0: a duplicate is not an unattributed row, and counting it as one would block every inferred zero",
			res.Unidentified)
	}
}

// Replay: IngestVS already took periodKey as an explicit argument, but
// ObservedAt was still stamped from time.Now() internally — the same defect
// TestIngestRosterStampsFactsWithTheCapturesStartedAtNotWallClockNow checks
// for the roster route. startedAt is set six years in the past so a
// wall-clock fallback would fail regardless of what day this test runs.
// Covers both a read fact and an inferred zero, since they take different
// code paths to InsertFact.
func TestIngestVSStampsFactsWithTheCapturesStartedAtNotWallClockNow(t *testing.T) {
	h := newVSIngestHarness(t, vsFixture{captureComplete: true, rosterSize: 3, rankedRows: 2})
	past := time.Date(2020, 1, 15, 9, 30, 0, 0, time.UTC)
	h.store.startedAt = past

	if _, err := h.IngestVS(context.Background(), 1, "2020-W03"); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	var sawRead, sawInferredZero bool
	for _, f := range h.store.Facts {
		if f.Metric != "vs_points" {
			continue
		}
		if !f.ObservedAt.Equal(past) {
			t.Errorf("fact %+v ObservedAt = %v, want the capture's started_at %v, not wall-clock now", f, f.ObservedAt, past)
		}
		if f.PeriodKey != "2020-W03" {
			t.Errorf("fact %+v PeriodKey = %q, want 2020-W03", f, f.PeriodKey)
		}
		if f.Value == 0 {
			sawInferredZero = true
		} else {
			sawRead = true
		}
	}
	if !sawRead || !sawInferredZero {
		t.Fatalf("expected both a read fact and an inferred zero in this fixture, sawRead=%v sawInferredZero=%v", sawRead, sawInferredZero)
	}
}

// Two members the matcher cannot tell apart are a hazard no threshold fixes
// and only an alias can, so ingest says so out loud rather than silently
// attributing one of them.
func TestIngestVSWarnsWhenTwoMembersAreIndistinguishable(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newVSHarness(t, "complete")
	// ALBAN80 and ALBANSO differ by one confusable substitution, which
	// confusable.go charges 2 tenths of an edit -- they score above
	// AutoAccept against each other.
	for _, name := range []string{"ALBAN80", "ALBANSO"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(1), 0)
	h.engine.Results = []ocr.Result{
		{Text: "ALBAN80", Confidence: 0.95}, {Text: "ALBAN80", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	log := buf.String()
	if !strings.Contains(log, "indistinguishable") {
		t.Errorf("no warning logged for two members scoring above AutoAccept against each other; log was:\n%s", log)
	}
	// Not just that some warning fired: that it names the right two members,
	// rather than a garbled or empty pair the "indistinguishable" substring
	// check alone cannot tell apart from a correct one.
	if !strings.Contains(log, "ALBAN80") || !strings.Contains(log, "ALBANSO") {
		t.Errorf("warning did not name both indistinguishable members (ALBAN80, ALBANSO); log was:\n%s", log)
	}
}

// A value that parses and sits in a window CLOSED on both sides is
// corroborated by the ordering, so it no longer rests on OCR confidence
// alone. Measured on capture 6: two of the six points failures were EXACTLY
// RIGHT and rejected for confidence -- 8,835,180 at 0.52 and 1,242,375 at
// 0.70.
func TestIngestVSAcceptsALowConfidencePointsReadThatSitsInOrder(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		{Text: "Bravo02", Confidence: 0.95}, {Text: "Bravo02", Confidence: 0.95}, {Text: "8,000,000", Confidence: 0.52},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if len(h.store.Facts) != 3 {
		t.Errorf("wrote %d facts, want 3: 8,000,000 sits between its neighbours (a window closed on both sides) and is corroborated by the ordering", len(h.store.Facts))
	}
	if reasons := h.reviewReasons(); reasons["low_confidence_points"] != 0 {
		t.Errorf("an in-order, doubly-bracketed value was rejected on OCR confidence alone; all reasons: %v", reasons)
	}
	// Not just that a fact was written, but that it was written at the
	// promoted confidence and not at its own weak 0.52: UpsertFact only
	// supersedes on a STRICTLY higher confidence, so a fact left at 0.52
	// could never be superseded by a later genuine read landing at exactly
	// factConfidenceGate.
	bravoID := h.store.members[1].ID // Bravo02
	var bravoConf float64 = -1
	for _, f := range h.store.Facts {
		if f.MemberID == bravoID {
			bravoConf = f.Confidence
		}
	}
	if bravoConf != factConfidenceGate {
		t.Errorf("Bravo02's promoted fact confidence = %v, want exactly factConfidenceGate (%v)", bravoConf, factConfidenceGate)
	}
}

// The other half, and the reason the first half is safe: a value that parses
// but violates its bounds is the signature of a crop that caught neighbouring
// content, which is exactly what vsPointsSpec's charset comment describes.
//
// The assertion is exact rather than merely "at least one", because a broken
// monotonicKnown can produce a rejection for the WRONG reason and still trip
// a "!= 0" check: with monotonicKnown deleted entirely, Bravo02's bogus 99M
// seeds Lo = 99,000,000 for Alpha01, so the correct rank-1 read (9,000,000)
// is ALSO rejected -- two rows queued and one fact written, not two -- and a
// loose assertion would not notice.
func TestIngestVSRejectsAPointsValueThatBreaksTheRankingOrder(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		// Rank 2 cannot outscore rank 1.
		{Text: "Bravo02", Confidence: 0.95}, {Text: "Bravo02", Confidence: 0.95}, {Text: "99,000,000", Confidence: 0.95},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if reasons := h.reviewReasons(); reasons["points_out_of_order"] != 1 {
		t.Errorf("points_out_of_order = %d, want exactly 1 (only Bravo02's own bogus read, not its neighbours too); all reasons: %v", reasons["points_out_of_order"], reasons)
	}
	if len(h.store.Facts) != 2 {
		t.Errorf("wrote %d facts, want 2: Alpha01 (9,000,000) and Charlie03 (7,000,000) are untouched by Bravo02's bad read", len(h.store.Facts))
	}
}

// The Critical-1 guard: an in-order read must not be promoted on the
// strength of an OPEN-ended window, because withinBounds is unconditionally
// true against an open end and proves nothing about correctness -- it only
// proves the parser matched, which the old low_confidence_points guard
// already established on its own. Alpha01 sits at rank 1, where Hi is always
// open (nothing ranks above it), so a weak Alpha01 read must still queue
// even though it is numerically "in order" relative to its one real
// neighbour below.
func TestIngestVSDoesNotPromoteAnOpenEndedWindow(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		// Rank 1: reads fine but weakly, and its window is open above.
		{Text: "Alpha01", Confidence: 0.95}, {Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.52},
		{Text: "Bravo02", Confidence: 0.95}, {Text: "Bravo02", Confidence: 0.95}, {Text: "8,000,000", Confidence: 0.95},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if reasons := h.reviewReasons(); reasons["low_confidence_points"] != 1 {
		t.Errorf("low_confidence_points = %d, want 1: rank 1's window is open above and must not be promoted; all reasons: %v", reasons["low_confidence_points"], reasons)
	}
	if len(h.store.Facts) != 2 {
		t.Errorf("wrote %d facts, want 2: Bravo02 and Charlie03 only, Alpha01 held for review", len(h.store.Facts))
	}
}

// There used to be a Critical-1-adjacent guard here: a window closed on BOTH
// sides was still not enough on its own, and the read's own OCR confidence
// also had to clear pointsOrderConfidenceFloor (0.40). Measured against
// capture 6 (investigation-confidence-floor.md), the only row that floor
// ever excluded on real data was correct (rank 10, confidence 0.0853), and
// the floor caught nothing wrong. It was removed, so a window closed on both
// sides now promotes regardless of how low the read's own confidence is --
// this test proves that directly, at a confidence (0.09) below where the old
// floor sat.
func TestIngestVSPromotesAClosedWindowReadEvenAtVeryLowConfidence(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		// Rank 2's window is closed on both sides (bracketed by Alpha01 and
		// Charlie03) and its own OCR confidence (0.09) is far below the old,
		// now-removed 0.40 floor -- the shape of capture 6's rank 10.
		{Text: "Bravo02", Confidence: 0.95}, {Text: "Bravo02", Confidence: 0.95}, {Text: "8,000,000", Confidence: 0.09},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if reasons := h.reviewReasons(); reasons["low_confidence_points"] != 0 {
		t.Errorf("low_confidence_points = %d, want 0: Bravo02's window is closed on both sides, and there is no confidence floor left to block it; all reasons: %v", reasons["low_confidence_points"], reasons)
	}
	if len(h.store.Facts) != 3 {
		t.Errorf("wrote %d facts, want 3: Alpha01, Bravo02 and Charlie03 all write", len(h.store.Facts))
	}
	bravoID := h.store.members[1].ID
	f, ok := h.factFor(bravoID)
	if !ok {
		t.Fatalf("Bravo02 has no fact: its closed-window 8,000,000 read must have been promoted, not queued")
	}
	if f.Value != 8000000 {
		t.Errorf("Bravo02's fact = %v, want 8,000,000", f.Value)
	}
	// Promoted to exactly factConfidenceGate, not left at its own weak 0.09 --
	// same requirement TestIngestVSAcceptsALowConfidencePointsReadThatSitsInOrder
	// makes for the 0.52 case; this is the same promotion path at a much lower
	// starting confidence, which is the whole point of removing the floor.
	if f.Confidence != factConfidenceGate {
		t.Errorf("Bravo02's promoted fact confidence = %v, want exactly factConfidenceGate (%v)", f.Confidence, factConfidenceGate)
	}
}

// The empty points read is the same PSM 7 layout blindness the name field
// retries for. It is allowed here only because the value has to land inside
// the window its neighbours define -- a manufactured number has no reason to.
func TestIngestVSRetriesAnEmptyPointsReadAndAcceptsAnInOrderValue(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		// Empty at the primary PSM, read at the retry.
		{Text: "Bravo02", Confidence: 0.95}, {Text: "Bravo02", Confidence: 0.95}, {Text: "", Confidence: 0},
		{Text: "8,000,000", Confidence: 0.90},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	// Asserted per member, not as a bare count: a count comparison alone
	// cannot distinguish "all three rows wrote, one at a wrong value" from
	// "only two rows wrote" if a third, unrelated fact happened to appear --
	// and it is exactly as blind to a swap between two members. Each of the
	// three must exist AND carry its own value.
	want := map[int64]float64{
		h.store.members[0].ID: 9000000, // Alpha01: read cleanly by the primary
		h.store.members[1].ID: 8000000, // Bravo02: empty at the primary, read by the retry
		h.store.members[2].ID: 7000000, // Charlie03: read cleanly by the primary
	}
	for id, wantValue := range want {
		f, ok := h.factFor(id)
		if !ok {
			t.Errorf("member %d: no fact written, want %v", id, wantValue)
			continue
		}
		if f.Value != wantValue {
			t.Errorf("member %d: fact = %v, want %v", id, f.Value, wantValue)
		}
	}
	if len(h.store.Facts) != 3 {
		t.Errorf("wrote %d facts, want exactly 3", len(h.store.Facts))
	}
}

// The guard that makes the retry defensible at all. A raw-line retry on a crop
// that caught neighbouring content can produce a well-formed number, and
// nothing about the number itself says so -- only its position does.
func TestIngestVSRejectsARetriedPointsValueThatBreaksTheOrder(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		{Text: "Bravo02", Confidence: 0.95}, {Text: "Bravo02", Confidence: 0.95}, {Text: "", Confidence: 0},
		{Text: "44,357,000", Confidence: 0.90}, // well-formed, and impossible at rank 2
		{Text: "Charlie03", Confidence: 0.95}, {Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if reasons := h.reviewReasons(); reasons["points_out_of_order"] == 0 {
		t.Errorf("a fabricated but well-formed retry value was written; all reasons: %v", reasons)
	}
	// The reason-count check above cannot tell "Bravo02 was rejected" from
	// "some unrelated row was queued as points_out_of_order and Bravo02's
	// fabrication was written anyway" -- checking that Bravo02 specifically
	// has no fact, and that its two neighbours are untouched by its bad read,
	// closes that gap.
	if _, ok := h.factFor(h.store.members[1].ID); ok {
		t.Errorf("Bravo02 has a fact: its fabricated 44,357,000 must have been rejected, not written")
	}
	for id, wantValue := range map[int64]float64{
		h.store.members[0].ID: 9000000,
		h.store.members[2].ID: 7000000,
	} {
		f, ok := h.factFor(id)
		if !ok {
			t.Errorf("member %d: no fact written, want %v", id, wantValue)
			continue
		}
		if f.Value != wantValue {
			t.Errorf("member %d: fact = %v, want %v", id, f.Value, wantValue)
		}
	}
}

// The controller ruling: a retried value must never seed a bound, even when
// its own reported confidence clears factConfidenceGate. Bravo02's retried
// read is a well-formed, high-confidence, individually-in-range number -- so
// if it were allowed to seed, it would hand rank 3 a Hi ceiling of 1,000,000
// and reject Charlie03's genuine 7,000,000 read as out of order. Charlie03
// must be written untouched, and Bravo02's own fabricated value is still
// judged (and rejected) against the window its REAL neighbours define.
func TestIngestVSNeverSeedsABoundFromARetriedRead(t *testing.T) {
	h := newVSHarness(t, "complete")
	for _, name := range []string{"Alpha01", "Bravo02", "Charlie03"} {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Active: true,
		})
	}
	h.addFrame(vsFrame(3), 0)
	h.engine.Results = []ocr.Result{
		{Text: "Alpha01", Confidence: 0.95}, {Text: "Alpha01", Confidence: 0.95}, {Text: "9,000,000", Confidence: 0.95},
		// Empty at the primary PSM; the retry reads a well-formed, confident,
		// but fabricated value that a crop catching neighbouring content could
		// plausibly produce. Bracketed by Alpha01 (9,000,000) and Charlie03
		// (7,000,000) it is itself out of order and must be rejected -- but the
		// real question this test asks is whether it also corrupts Charlie03.
		{Text: "Bravo02", Confidence: 0.95}, {Text: "Bravo02", Confidence: 0.95}, {Text: "", Confidence: 0},
		{Text: "1,000,000", Confidence: 0.90},
		{Text: "Charlie03", Confidence: 0.95}, {Text: "Charlie03", Confidence: 0.95}, {Text: "7,000,000", Confidence: 0.95},
	}

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if reasons := h.reviewReasons(); reasons["points_out_of_order"] != 1 {
		t.Errorf("points_out_of_order = %d, want exactly 1 (Bravo02's own fabricated read); all reasons: %v", reasons["points_out_of_order"], reasons)
	}
	// Asserted per member, not as a bare count. Without the never-seed guard,
	// Bravo02's 1,000,000 DOES seed a bound -- and the row it corrupts is
	// Charlie03's, not Bravo02's own: monotonicKnown takes the longest
	// non-increasing run over the seeds [9M, 1M, 7M], keeps {9M, 1M} on the
	// first-found tie-break, and drops Charlie03's 7M instead, so Charlie03
	// is the one queued as points_out_of_order and Bravo02 is the one
	// written. `len(Facts) != 2` and a reason count of exactly 1 both still
	// hold in that broken world, and a loop that only inspects facts it
	// finds for Charlie03's ID never fires, because there is no such fact to
	// find -- every assertion in this test used to survive that mutation
	// silently. Checking existence directly is what catches it: with the
	// guard removed, factFor(Charlie03) below returns ok=false and this test
	// fails.
	if f, ok := h.factFor(h.store.members[1].ID); ok {
		t.Errorf("Bravo02 has a fact (%v): its fabricated 1,000,000 must have been rejected, not written", f.Value)
	}
	if f, ok := h.factFor(h.store.members[0].ID); !ok {
		t.Errorf("Alpha01 has no fact: its genuine 9,000,000 read must not be affected by Bravo02's retry")
	} else if f.Value != 9000000 {
		t.Errorf("Alpha01's fact = %v, want 9,000,000", f.Value)
	}
	f, ok := h.factFor(h.store.members[2].ID)
	if !ok {
		t.Fatalf("Charlie03 has no fact at all: Bravo02's retried read seeded a bound and pushed Charlie03 out of order instead")
	}
	if f.Value != 7000000 {
		t.Errorf("Charlie03's fact = %v, want 7,000,000: Bravo02's retried 1,000,000 must not have capped it", f.Value)
	}
	if len(h.store.Facts) != 2 {
		t.Errorf("wrote %d facts, want exactly 2: Alpha01 and Charlie03", len(h.store.Facts))
	}
}

// Both segmentation modes run on every name crop and the better score wins.
// Their miss sets are disjoint in four places on capture 6, so this is not a
// tie-break, it is two independent readings of the same pixels.
func TestIngestVSTakesTheBetterOfTwoNameReads(t *testing.T) {
	h := newVSHarness(t, "complete")
	h.store.nextMemberID++
	h.store.members = append(h.store.members, db.Member{
		ID: h.store.nextMemberID, AllianceID: 1, Name: "Alpha01",
		NameNormalized: roster.Normalize("Alpha01"), Active: true,
	})
	h.addFrame(vsFrame(1), 0)
	h.engine.Results = []ocr.Result{
		{Text: "XXXXXXX", Confidence: 0.95}, // primary mode: matches nobody
		{Text: "Alpha01", Confidence: 0.95}, // second mode: exact
		{Text: "9,000,000", Confidence: 0.95},
	}

	res, err := h.IngestVS(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Matched != 1 {
		t.Errorf("matched %d, want 1: the second mode read the name exactly", res.Matched)
	}
}
