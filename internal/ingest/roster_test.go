package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
	"github.com/tomharris/lw-manager/internal/vision"
)

// testPeriodKey is the periodKey most tests in this file pass to
// IngestRoster — its exact form doesn't matter to them, only that it round-
// trips onto the facts written, which TestIngestRosterWritesFactsWithScreenshotProvenance
// and the replay-specific tests below check directly.
const testPeriodKey = "2026-08-01"

// --- fakes -------------------------------------------------------------

// fakeIngestStore is an in-memory Store: no Postgres, per the
// replay-before-real discipline this project follows everywhere.
type fakeIngestStore struct {
	frames     []db.CaptureFrame
	members    []db.Member
	aliases    map[int64][]string
	objectKeys map[int64]string
	allianceID int64
	startedAt  time.Time

	// currentAllianceErr, when set, is what CurrentAllianceID returns instead
	// of allianceID — the fresh-deployment case task 19 exists to fix, where
	// nothing has ever written the alliances table.
	currentAllianceErr error

	nextMemberID int64

	Facts          []db.Fact
	Reviews        []db.ReviewItem
	MembersCreated int
	FinishedStatus string
	FinishedParsed int

	// MemberCountSet and MemberCountSetCalls record every
	// SetAllianceMemberCount call, so a test can assert both the final
	// value and (via the call count) that it was written at all.
	MemberCountSet      int
	MemberCountSetCalls int
}

func (s *fakeIngestStore) Capture(ctx context.Context, id int64) (db.Capture, error) {
	return db.Capture{ID: id, StartedAt: s.startedAt}, nil
}

func (s *fakeIngestStore) CaptureFrames(ctx context.Context, captureID int64) ([]db.CaptureFrame, error) {
	return s.frames, nil
}

func (s *fakeIngestStore) ScreenshotObjectKey(ctx context.Context, screenshotID int64) (string, error) {
	key, ok := s.objectKeys[screenshotID]
	if !ok {
		return "", fmt.Errorf("fake store: no object key for screenshot %d", screenshotID)
	}
	return key, nil
}

func (s *fakeIngestStore) ListMembers(ctx context.Context, allianceID int64) ([]db.Member, error) {
	return s.members, nil
}

func (s *fakeIngestStore) MemberAliases(ctx context.Context, allianceID int64) (map[int64][]string, error) {
	return s.aliases, nil
}

func (s *fakeIngestStore) CreateMember(ctx context.Context, m db.Member) (int64, error) {
	s.nextMemberID++
	m.ID = s.nextMemberID
	s.members = append(s.members, m)
	s.MembersCreated++
	return m.ID, nil
}

func (s *fakeIngestStore) InsertFact(ctx context.Context, f db.Fact) (int64, error) {
	s.Facts = append(s.Facts, f)
	return int64(len(s.Facts)), nil
}

func (s *fakeIngestStore) QueueReview(ctx context.Context, r db.ReviewItem) (int64, error) {
	s.Reviews = append(s.Reviews, r)
	return int64(len(s.Reviews)), nil
}

func (s *fakeIngestStore) FinishCapture(ctx context.Context, id int64, status string, parsed int, errMsg string) error {
	s.FinishedStatus = status
	s.FinishedParsed = parsed
	return nil
}

func (s *fakeIngestStore) CurrentAllianceID(ctx context.Context) (int64, error) {
	if s.currentAllianceErr != nil {
		return 0, s.currentAllianceErr
	}
	return s.allianceID, nil
}

func (s *fakeIngestStore) SetAllianceMemberCount(ctx context.Context, allianceID int64, count int) error {
	s.MemberCountSet = count
	s.MemberCountSetCalls++
	return nil
}

// fakeBlobs is an in-memory blob.Store.
type fakeBlobs struct {
	objects map[string][]byte
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{objects: map[string][]byte{}} }

func (b *fakeBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	b.objects[key] = data
	return nil
}

func (b *fakeBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	data, ok := b.objects[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *fakeBlobs) Exists(ctx context.Context, key string) (bool, error) {
	_, ok := b.objects[key]
	return ok, nil
}

// --- harness -------------------------------------------------------------

// rosterIngestHarness wires an Ingester over the three fakes above.
// Embedding *Ingester lets a test call h.IngestRoster(...) directly.
type rosterIngestHarness struct {
	*Ingester
	t                *testing.T
	store            *fakeIngestStore
	blobs            *fakeBlobs
	engine           *ocr.FakeEngine
	nextScreenshotID int64

	// allianceFrameID is the screenshot id of the alliance-summary frame
	// addAllianceFrame (or newRosterIngestHarness's allianceMemberCountText
	// fixture field) added, or 0 if none was added — a test can use it to
	// confirm that frame never appears attributed to a fact or review row.
	allianceFrameID int64
}

func newHarness(t *testing.T) *rosterIngestHarness {
	t.Helper()
	store := &fakeIngestStore{
		objectKeys: map[int64]string{},
		aliases:    map[int64][]string{},
		allianceID: 1,
	}
	blobs := newFakeBlobs()
	engine := &ocr.FakeEngine{}
	return &rosterIngestHarness{
		Ingester: New(store, blobs, engine),
		t:        t,
		store:    store,
		blobs:    blobs,
		engine:   engine,
	}
}

// addFrame PNG-encodes img, files it under a fresh screenshot id in the fake
// blob store, and appends a capture_frames row at offsetPx. Returns the
// screenshot id.
func (h *rosterIngestHarness) addFrame(img image.Image, offsetPx int) int64 {
	h.t.Helper()
	h.nextScreenshotID++
	id := h.nextScreenshotID

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		h.t.Fatalf("encoding synthetic frame: %v", err)
	}
	key := fmt.Sprintf("frame-%d", id)
	h.store.objectKeys[id] = key
	h.blobs.objects[key] = buf.Bytes()

	h.store.frames = append(h.store.frames, db.CaptureFrame{
		ID: id, CaptureID: 1, Seq: len(h.store.frames), ScreenshotID: id, OffsetPx: offsetPx,
	})
	return id
}

// addAllianceFrame appends the alliance screen's summary frame, tagged
// vision.AllianceSummaryGroupKey exactly as roster_capture.go tags it, so it
// exercises the same "not a list frame, must never be segmented" path
// IngestRoster gives that tag in production. The image content is
// irrelevant -- ocr.FakeEngine never looks at the pixels, only SegmentRows
// does, and this frame must never reach SegmentRows at all.
//
// It must be called before the frame(s) it should precede: IngestRoster
// reads the alliance frame before it reads any group header, so its OCR
// result must be scripted first too (see newRosterIngestHarness's
// allianceMemberCountText handling).
func (h *rosterIngestHarness) addAllianceFrame() int64 {
	h.t.Helper()
	id := h.addFrame(rosterFrame(0), 0)
	h.store.frames[len(h.store.frames)-1].GroupKey = vision.AllianceSummaryGroupKey
	h.allianceFrameID = id
	return id
}

// rosterFrame builds a synthetic member-list frame with nRows cards in
// memberListRegion at memberRowPitch, tall enough that none are clipped by
// the region's lower edge. ocr.FakeEngine never looks at the pixels — only
// SegmentRows does, so this only has to be geometrically right, not
// visually realistic (see the recon-measured field rects' own doc comment
// in roster.go for why pixel realism is not this package's job to fake).
//
// imgH is sized so the scanned region wraps the drawn cards with about one
// spare pitch, not thousands of blank pixels: phase-locking measures
// periodicity across the whole scanned region (see segment.go), and a vast
// flat trailer -- which a real frame does not have, since a short group is
// followed immediately by the next group's header or more list content, not
// empty page -- would dilute that signal in a way no real capture does.
func rosterFrame(nRows int) image.Image {
	imgH := 400
	for {
		top := int(memberListRegion.Y1 * float64(imgH))
		bot := int(memberListRegion.Y2 * float64(imgH))
		if bot-top >= (nRows+1)*memberRowPitch {
			break
		}
		imgH += 200
	}
	top := int(memberListRegion.Y1 * float64(imgH))
	return cardFrame(200, imgH, top, 100, 12, nRows)
}

// rosterFixture describes one IngestRoster scenario in terms of the rows a
// single synthetic frame should carry. Rows are scripted in the exact order
// IngestRoster reads them: one group-header read, then per row
// name/power/level/last-active.
type rosterFixture struct {
	group      string
	groupTotal int

	// existing pre-registers N real members (Member01..) and scripts N rows
	// whose OCR'd name matches one of them exactly (score 100, auto-accept).
	existing int
	// extraGarbled appends N rows whose name matches no known member at all.
	extraGarbled int
	// parsedRows, if the fixture has specified none of the above, fills the
	// frame with N generic rows that each create a new member.
	parsedRows int
	// ambiguousName appends one row, "kaln445", scored against a registered
	// "kain445" -- one substitution in a 7-char name lands at 85, squarely
	// in the 75-92 review band (see roster.TokenSetRatio's own fixture).
	ambiguousName bool
	// unparseablePower appends one row whose power field is not
	// shape-valid, so ParsePower returns ErrUnparseable.
	unparseablePower bool
	// lowConfidencePower appends one row whose power field parses fine but
	// whose OCR confidence (0.5) blends below factConfidenceGate even
	// against a fresh (matchNorm 1.0) member -- for the write-time
	// confidence gate, distinct from unparseablePower's shape failure.
	lowConfidencePower bool

	// allianceMemberCountText, when non-empty, prepends an alliance-summary
	// frame (vision.AllianceSummaryGroupKey) scripted with this raw
	// "Members: X/Y" OCR text, ahead of the group frame -- exercising the
	// alliance-total reconciliation path. Empty (the default, and every
	// fixture that predates this field) means no alliance frame at all,
	// matching a capture recorded before this check existed and exercising
	// IngestRoster's missing-frame fallback.
	allianceMemberCountText string
}

type rowScript struct {
	name, power, level, lastActive           string
	nameConf, powerConf, levelConf, lastConf float64
}

func newRosterIngestHarness(t *testing.T, fx rosterFixture) *rosterIngestHarness {
	t.Helper()
	h := newHarness(t)

	group := fx.group
	if group == "" {
		group = "R1"
	}

	var rows []rowScript
	for k := 0; k < fx.existing; k++ {
		name := fmt.Sprintf("Member%02d", k+1)
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: name,
			NameNormalized: roster.Normalize(name), Rank: group, Active: true,
		})
		rows = append(rows, rowScript{
			name: name, power: "Power: 200.0M", level: "Lv.30", lastActive: "Online",
			nameConf: 0.95, powerConf: 0.95, levelConf: 0.95, lastConf: 0.95,
		})
	}
	for k := 0; k < fx.extraGarbled; k++ {
		rows = append(rows, rowScript{
			name: "Q1", power: "Power: 200.0M", level: "Lv.30", lastActive: "Online",
			nameConf: 0.5, powerConf: 0.9, levelConf: 0.9, lastConf: 0.9,
		})
	}
	if fx.ambiguousName {
		h.store.nextMemberID++
		h.store.members = append(h.store.members, db.Member{
			ID: h.store.nextMemberID, AllianceID: 1, Name: "kain445",
			NameNormalized: roster.Normalize("kain445"), Rank: group, Active: true,
		})
		rows = append(rows, rowScript{
			name: "kaln445", power: "Power: 200.0M", level: "Lv.30", lastActive: "Online",
			nameConf: 0.8, powerConf: 0.9, levelConf: 0.9, lastConf: 0.9,
		})
	}
	if fx.unparseablePower {
		rows = append(rows, rowScript{
			name: "GenericName", power: "???", level: "Lv.30", lastActive: "Online",
			nameConf: 0.9, powerConf: 0.3, levelConf: 0.9, lastConf: 0.9,
		})
	}
	if fx.lowConfidencePower {
		rows = append(rows, rowScript{
			name: "LowConfName", power: "Power: 200.0M", level: "Lv.30", lastActive: "Online",
			nameConf: 0.9, powerConf: 0.5, levelConf: 0.9, lastConf: 0.9,
		})
	}
	if fx.parsedRows > 0 && len(rows) == 0 {
		for k := 0; k < fx.parsedRows; k++ {
			// k*7919 (prime) spreads consecutive indices across several
			// digits, so no two generated names land near each other in
			// roster.TokenSetRatio the way "Generic000"/"Generic001" would
			// (one digit apart, well inside the 75-92 ambiguous band) — this
			// fixture is about row-count reconciliation, not name matching.
			rows = append(rows, rowScript{
				name: fmt.Sprintf("Generic%06d", k*7919), power: "Power: 200.0M", level: "Lv.30", lastActive: "Online",
				nameConf: 0.9, powerConf: 0.9, levelConf: 0.9, lastConf: 0.9,
			})
		}
	}

	var results []ocr.Result
	if fx.allianceMemberCountText != "" {
		// Added, and scripted, ahead of the group frame: IngestRoster reads
		// the alliance frame (readAllianceMemberCount) before it reads any
		// group header.
		h.addAllianceFrame()
		results = append(results, ocr.Result{Text: fx.allianceMemberCountText, Confidence: 0.9})
	}

	h.addFrame(rosterFrame(len(rows)), 0)

	results = append(results, ocr.Result{Text: fmt.Sprintf("%s Group %d/%d", group, fx.groupTotal, fx.groupTotal), Confidence: 0.9})
	for _, r := range rows {
		results = append(results,
			ocr.Result{Text: r.name, Confidence: r.nameConf},
			ocr.Result{Text: r.power, Confidence: r.powerConf},
			ocr.Result{Text: r.level, Confidence: r.levelConf},
			ocr.Result{Text: r.lastActive, Confidence: r.lastConf},
		)
	}
	h.engine.Results = results

	return h
}

// distinctRowNames are eleven names with no shared root or numeric suffix,
// so no pair of them can accidentally land in roster.TokenSetRatio's
// ambiguous band the way "Row00" vs "Row01" (one digit apart) would —
// used wherever a test needs several rows that must each create a distinct
// member rather than match or ambiguously collide with one another.
var distinctRowNames = []string{
	"Zephyr", "Quokka", "Umbrella", "Xylophone", "Yonderland", "Cascade",
	"Thunderbolt", "Falconry", "Meadowlark", "Granitepeak", "Ripplewave",
}

// rowResults scripts one row's four field reads, in IngestRoster's call
// order, all comfortably valid and high-confidence.
func rowResults(name string) []ocr.Result {
	return []ocr.Result{
		{Text: name, Confidence: 0.9},
		{Text: "Power: 200.0M", Confidence: 0.9},
		{Text: "Lv.30", Confidence: 0.9},
		{Text: "Online", Confidence: 0.9},
	}
}

// --- tests -----------------------------------------------------------------

// The structural guard the recon supplied: if a group states 11 members and
// eleven already matched, a twelfth is an OCR artifact rather than a person.
// This is a check where the alternative is a tuned threshold.
func TestIngestRosterRefusesToCreateBeyondTheGroupCount(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group:        "R2",
		groupTotal:   11,
		existing:     11,
		extraGarbled: 1,
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 0 {
		t.Errorf("created %d members, want 0 — the group is already full", res.Created)
	}
	if res.Queued != 1 {
		t.Errorf("queued %d for review, want 1", res.Queued)
	}
}

func TestIngestRosterMarksAShortGroupPartial(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{group: "R3", groupTotal: 64, parsedRows: 60})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Status != "partial" {
		t.Errorf("status = %q, want partial", res.Status)
	}
	if got := res.PerGroup["R3"]; got.Expected != 64 || got.Parsed != 60 {
		t.Errorf("R3 tally = %+v, want {64 60}", got)
	}
}

func TestIngestRosterQueuesALowConfidenceNameRatherThanGuessing(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R2", groupTotal: 11, existing: 11, ambiguousName: true,
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Queued == 0 {
		t.Error("an ambiguous name must reach review, never a leaderboard")
	}
}

func TestIngestRosterWritesFactsWithScreenshotProvenance(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{group: "R2", groupTotal: 2, existing: 2})

	if _, err := h.IngestRoster(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	for _, f := range h.store.Facts {
		if f.ScreenshotID == 0 {
			t.Errorf("fact %+v has no screenshot reference", f)
		}
		if f.Confidence <= 0 {
			t.Errorf("fact %+v has no confidence", f)
		}
	}
}

// Finding 1 (M4 task-11 follow-up): an unparseable field must not cost the
// whole row. The row's name and its other two numeric fields all read fine,
// so the member is still resolved and two of its three facts still land —
// only the field that failed to parse is queued for review, carrying that
// field's own OCR text, not the row's.
func TestIngestRosterWritesTheFieldsThatParsedWhenOneDoesNot(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R1", groupTotal: 5, unparseablePower: true,
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 1 {
		t.Errorf("created %d members, want 1 — the name and the two good fields must still resolve the row", res.Created)
	}
	if res.Queued != 1 {
		t.Errorf("queued %d for review, want 1 (only the unparseable field)", res.Queued)
	}
	if len(h.store.Facts) != 2 {
		t.Fatalf("wrote %d facts, want 2 (level and last_active_hours; power failed to parse)", len(h.store.Facts))
	}
	for _, f := range h.store.Facts {
		if f.Metric == "power" {
			t.Errorf("a power fact was written despite an unparseable power field: %+v", f)
		}
	}
	if len(h.store.Reviews) != 1 {
		t.Fatalf("queued %d review rows, want 1", len(h.store.Reviews))
	}
	if got := h.store.Reviews[0].RawText; got != "???" {
		t.Errorf("review raw_text = %q, want the power field's own OCR text %q, not the row's name", got, "???")
	}
}

// The second consequence of field-level review: a row that produced some
// facts must not be counted as a failed row for reconciliation. Group tally
// counts rows segmented against the header's stated total, independent of
// how many of a row's fields resolved to facts — reconciliation exists to
// catch rows that were never photographed at all, and a per-field OCR
// failure further downstream must not be confused with that (see
// GroupTally's own doc comment).
func TestIngestRosterCountsAPartiallyParsedRowTowardReconciliation(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R1", groupTotal: 1, unparseablePower: true,
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Status != "complete" {
		t.Errorf("status = %q, want complete — the row was parsed, just incompletely", res.Status)
	}
	if got := res.PerGroup["R1"]; got.Parsed != 1 || got.Expected != 1 {
		t.Errorf("R1 tally = %+v, want {1 1}", got)
	}
}

// Finding 2 (M4 task-11 follow-up): CLAUDE.md invariant #5 says a
// low-confidence read goes to the review queue, never to a leaderboard.
// Facts are what a leaderboard reads, so a blended confidence below 0.80
// must not produce a fact at all. The negative is asserted directly — a
// fact quietly appearing alongside the review row is the failure mode a
// check of the review row alone would not catch.
func TestIngestRosterNeverWritesAFactBelowTheConfidenceGate(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R1", groupTotal: 5, lowConfidencePower: true,
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	for _, f := range h.store.Facts {
		if f.Metric == "power" {
			t.Fatalf("a power fact was written at a sub-gate confidence: %+v", f)
		}
	}
	if len(h.store.Facts) != 2 {
		t.Errorf("wrote %d facts, want 2 (level and last_active_hours; power was below the confidence gate)", len(h.store.Facts))
	}
	var found bool
	for _, r := range h.store.Reviews {
		if r.Reason == "low_confidence_power" {
			found = true
			if r.Confidence <= 0 || r.Confidence >= factConfidenceGate {
				t.Errorf("review confidence = %v, want in (0, %v)", r.Confidence, factConfidenceGate)
			}
			if r.RawText != "Power: 200.0M" {
				t.Errorf("review raw_text = %q, want the power field's own OCR text", r.RawText)
			}
		}
	}
	if !found {
		t.Error("no low_confidence_power review row queued")
	}
	if res.Queued == 0 {
		t.Error("a sub-gate confidence must reach review, never a leaderboard")
	}
}

// The sticky group header pins over whatever content is at the top of the
// scroll region once a group's second and later frames are captured, so
// that band is a row cut in half. It must be discarded rather than parsed,
// and discarding it must not perturb the geometric dedupe count: two frames
// scrolled by exactly one screenful apart must total the true number of
// distinct rows, not one fewer (double-dropped) or one more (double-counted).
func TestIngestRosterDiscardsTheOccludedTopRow(t *testing.T) {
	h := newHarness(t)

	frame1 := rosterFrame(6)
	frame2 := rosterFrame(6)
	h.addFrame(frame1, 0)   // first frame of the group: header has not pinned over anything yet
	h.addFrame(frame2, 672) // scrolled by exactly 6 rows * 112px pitch

	var results []ocr.Result
	results = append(results, ocr.Result{Text: "R1 Group 20/20", Confidence: 0.9})
	for k := 0; k < 6; k++ {
		results = append(results, rowResults(distinctRowNames[k])...)
	}
	results = append(results, ocr.Result{Text: "R1 Group 20/20", Confidence: 0.9})
	// Frame 2 detects 6 bands but its first (index 0) is occluded and must
	// never be OCR'd -- only 5 more rows are scripted here.
	for k := 6; k < 11; k++ {
		results = append(results, rowResults(distinctRowNames[k])...)
	}
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v (an unexpected OCR call means the occluded row was not dropped)", err)
	}
	if got := res.PerGroup["R1"].Parsed; got != 11 {
		t.Errorf("parsed %d rows across two frames, want 11 (6 + 5, the occluded top row of frame 2 dropped)", got)
	}
	if res.Created != 11 {
		t.Errorf("created %d members, want 11", res.Created)
	}
}

// Replay: a parser fix rerun over a capture from long ago must write the
// same facts it would have written on capture day, not today's. startedAt
// is set six years in the past and asserted directly against every fact's
// ObservedAt and PeriodKey — a test using a recent timestamp would pass
// whether or not IngestRoster actually used it, since "recent" and "now"
// are hard to tell apart.
func TestIngestRosterStampsFactsWithTheCapturesStartedAtNotWallClockNow(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{group: "R1", groupTotal: 1, parsedRows: 1})
	past := time.Date(2020, 1, 15, 9, 30, 0, 0, time.UTC)
	h.store.startedAt = past
	const replayPeriodKey = "2020-01-15"

	res, err := h.IngestRoster(context.Background(), 1, replayPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 1 {
		t.Fatalf("created %d members, want 1", res.Created)
	}
	if len(h.store.Facts) == 0 {
		t.Fatal("no facts were written")
	}
	for _, f := range h.store.Facts {
		if !f.ObservedAt.Equal(past) {
			t.Errorf("fact %+v ObservedAt = %v, want the capture's started_at %v, not wall-clock now", f, f.ObservedAt, past)
		}
		if f.PeriodKey != replayPeriodKey {
			t.Errorf("fact %+v PeriodKey = %q, want the derived replay period %q, not today's", f, f.PeriodKey, replayPeriodKey)
		}
	}
}

// --- M4 task-11b: the alliance-total reconciliation gap ------------------

// The whole point of the alliance-total check: a per-group loop can only
// judge groups it actually saw a frame for. A rank group whose frames never
// made it into the capture at all leaves no PerGroup entry to fall short of
// -- the per-group loop above finds nothing wrong -- but the sum of every
// group that *was* parsed still falls short of the alliance's own
// "Members: X/Y" count, and only the total check catches that.
func TestIngestRosterAllianceTotalCatchesAWholeMissingGroup(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R1", groupTotal: 2, parsedRows: 2,
		// The alliance says 5 members total; R1 alone (fully, internally
		// consistent at 2/2) accounts for only 2. The other 3 belong to a
		// group whose frames were never captured at all.
		allianceMemberCountText: "Members: 5/100",
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if got := res.PerGroup["R1"]; got.Expected != got.Parsed {
		t.Fatalf("R1 tally = %+v, want a fully self-consistent group -- that is the point of this test", got)
	}
	if res.Status != "partial" {
		t.Errorf("status = %q, want partial -- the alliance frame says 5 members but only 2 were parsed across the one group this capture saw", res.Status)
	}
	if !res.AllianceTotalChecked {
		t.Error("AllianceTotalChecked = false, want true -- the alliance frame was present and readable")
	}
	if res.AllianceMemberCount != 5 {
		t.Errorf("AllianceMemberCount = %d, want 5", res.AllianceMemberCount)
	}
}

// The alliance frame is not a list frame. It must never reach SegmentRows,
// so it must contribute no facts, no review rows, and no members -- only
// the group frame's own rows may.
func TestIngestRosterDoesNotSegmentTheAllianceFrame(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R1", groupTotal: 2, parsedRows: 2,
		allianceMemberCountText: "Members: 2/100",
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if h.allianceFrameID == 0 {
		t.Fatal("test setup error: no alliance frame was recorded")
	}
	if res.Created != 2 {
		t.Errorf("created %d members, want exactly 2 -- the group's own rows; the alliance frame must not have contributed any", res.Created)
	}
	if res.Queued != 0 {
		t.Errorf("queued %d for review, want 0", res.Queued)
	}
	for _, f := range h.store.Facts {
		if f.ScreenshotID == h.allianceFrameID {
			t.Errorf("fact %+v was attributed to the alliance frame, which is not a list frame and must never be segmented", f)
		}
	}
	for _, r := range h.store.Reviews {
		if r.ScreenshotID == h.allianceFrameID {
			t.Errorf("review %+v was attributed to the alliance frame, which is not a list frame and must never be segmented", r)
		}
	}
}

// A capture recorded before this check existed has no alliance frame at
// all. That must not be treated as a failure -- IngestRoster falls back to
// per-group reconciliation alone, exactly as it did before AllianceSummary
// frames existed.
func TestIngestRosterFallsBackToPerGroupWhenNoAllianceFrame(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{group: "R1", groupTotal: 2, parsedRows: 2})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.AllianceTotalChecked {
		t.Error("AllianceTotalChecked = true, want false -- no alliance frame was ever captured")
	}
	if res.Status != "complete" {
		t.Errorf("status = %q, want complete -- per-group reconciliation alone is consistent", res.Status)
	}
	if h.store.MemberCountSetCalls != 0 {
		t.Errorf("SetAllianceMemberCount called %d times, want 0 -- no member count was ever read", h.store.MemberCountSetCalls)
	}
}

// An alliance frame that is present but whose OCR text does not parse must
// degrade the same way a missing frame does -- not fail the whole capture.
// Losing an otherwise-good roster capture to one frame's bad OCR would cost
// more than the alliance-total check simply being unavailable this run.
func TestIngestRosterFallsBackToPerGroupWhenTheAllianceFrameIsUnreadable(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R1", groupTotal: 2, parsedRows: 2,
		allianceMemberCountText: "totally garbled, no digits here",
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v (an unreadable alliance frame must degrade, never fail the whole capture)", err)
	}
	if res.AllianceTotalChecked {
		t.Error("AllianceTotalChecked = true, want false -- the alliance frame's text did not parse")
	}
	if res.Status != "complete" {
		t.Errorf("status = %q, want complete -- per-group reconciliation alone is consistent, and an unreadable alliance frame must not turn that into partial", res.Status)
	}
	if h.store.MemberCountSetCalls != 0 {
		t.Errorf("SetAllianceMemberCount called %d times, want 0 -- nothing valid was ever read", h.store.MemberCountSetCalls)
	}
}

// The read alliance member count must persist to alliances.member_count, not
// only inform this run's reconciliation.
func TestIngestRosterWritesAllianceMemberCount(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R1", groupTotal: 2, parsedRows: 2,
		allianceMemberCountText: "Members: 2/100",
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Status != "complete" {
		t.Errorf("status = %q, want complete -- 2 parsed rows against an alliance count of 2", res.Status)
	}
	if h.store.MemberCountSetCalls != 1 {
		t.Fatalf("SetAllianceMemberCount called %d times, want exactly 1", h.store.MemberCountSetCalls)
	}
	if h.store.MemberCountSet != 2 {
		t.Errorf("alliance member count set to %d, want 2", h.store.MemberCountSet)
	}
}

// On a fresh deployment nothing has ever written the alliances table, so
// CurrentAllianceID returns db.ErrNotFound. Task 19 exists because that
// surfaced as an opaque "db: current alliance: db: not found" with no
// indication of what to do about it (see cmd/control ingest.go's error
// path); IngestRoster's wrap must name the fix — control alliance set —
// while still letting a caller recover the sentinel with errors.Is.
func TestIngestRosterNamesTheFixWhenNoAllianceHasEverBeenSet(t *testing.T) {
	h := newHarness(t)
	h.store.currentAllianceErr = fmt.Errorf("db: current alliance: %w", db.ErrNotFound)

	_, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err == nil {
		t.Fatal("IngestRoster: want an error when no alliance has ever been observed, got nil")
	}
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("IngestRoster error = %v, want it to still wrap db.ErrNotFound via %%w", err)
	}
	if !strings.Contains(err.Error(), "control alliance set") {
		t.Fatalf("IngestRoster error = %q, want it to name `control alliance set` as the fix", err.Error())
	}
}

// parseAllianceMemberCount reads the first number in "Members: X/Y" (the
// alliance's current headcount) and rejects text it cannot find that shape
// in -- an unparseable read must be distinguishable from a genuine zero.
func TestParseAllianceMemberCount(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "Members: 96/100", want: 96},
		{raw: "  Members: 5/50  ", want: 5},
		{raw: "Members: 0/100", want: 0},
		{raw: "garbled nonsense", wantErr: true},
		// The actual raw_text the first real ingest queued for review, back
		// when allianceMemberCountRegion's X2=0.60 cut off the value and left
		// only a scrap of the "Members:" label (task 21, evidence Finding 5).
		// This must keep failing to parse -- silently accepting it would be
		// worse than the review queue it went to.
		{raw: "4 ES", wantErr: true},
		// What the widened+tightened region (task 21) reads off a real
		// frame: capture 1's own alliance-summary screenshot and the
		// committed recon frame both produced this exact text via
		// TestPreprocMeasure, 2/2 (allianceMemberCountRegion's own comment
		// in roster.go).
		{raw: "Members: 97/100", want: 97},
		{raw: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseAllianceMemberCount(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAllianceMemberCount(%q) = %d, nil, want an error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAllianceMemberCount(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseAllianceMemberCount(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
