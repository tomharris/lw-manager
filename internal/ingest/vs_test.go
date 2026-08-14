package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
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
			points: strconv.Itoa(90000000 - k*100000),
		})
	}
	// Rows beyond the roster size cannot possibly match a real member —
	// used by the "never creates a member" test.
	for k := matched; k < fx.rankedRows; k++ {
		rows = append(rows, row{name: "ZzUnrecognizedGhostRow99", points: "1000000"})
	}
	if fx.duplicateSelfRow {
		rows = append(rows, row{name: "Member01", points: "5000000"})
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
			ocr.Result{Text: r.points, Confidence: 0.95},
		)
	}
	h.engine.Results = results

	return h
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
