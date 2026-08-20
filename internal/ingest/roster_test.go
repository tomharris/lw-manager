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
	factIndex      map[factKey]int // mirrors participation_facts' unique index; see UpsertFact
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

// factKey mirrors participation_facts' own unique constraint (member_id,
// metric, period_key, source, observed_at) — see UpsertFact's doc comment
// in internal/db/analytics.go for why the roster route can legitimately hit
// it twice.
type factKey struct {
	memberID           int64
	metric, periodKey  string
	source             string
	observedAtUnixNano int64
}

func factKeyOf(f db.Fact) factKey {
	return factKey{f.MemberID, f.Metric, f.PeriodKey, f.Source, f.ObservedAt.UnixNano()}
}

// UpsertFact simulates internal/db.Pool.UpsertFact's ON CONFLICT ... DO
// UPDATE ... WHERE EXCLUDED.confidence > participation_facts.confidence
// semantics in memory, rather than trusting the fake to blindly append the
// way InsertFact does — a test exercising task 27's fix needs the fake to
// actually enforce the same key collision Postgres does, or a regression
// here would pass this package's tests and still crash for real (the same
// replay-before-real discipline ReplayTransport follows for the device).
func (s *fakeIngestStore) UpsertFact(ctx context.Context, f db.Fact) (int64, bool, error) {
	key := factKeyOf(f)
	if s.factIndex == nil {
		s.factIndex = map[factKey]int{}
	}
	if idx, ok := s.factIndex[key]; ok {
		if f.Confidence <= s.Facts[idx].Confidence {
			return 0, false, nil // existing read is at least as good; no-op, same as Postgres' WHERE guard
		}
		s.Facts[idx] = f
		return int64(idx + 1), true, nil
	}
	s.Facts = append(s.Facts, f)
	s.factIndex[key] = len(s.Facts) - 1
	return int64(len(s.Facts)), true, nil
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

	// origMatchRankBadge remembers matchRankBadge's real value the first
	// time stubRankFor swaps it, so repeated calls (newHarness's own default,
	// then newRosterIngestHarness's fixture-specific one) register exactly
	// one t.Cleanup rather than a chain of them restoring each other's stub.
	origMatchRankBadge func(image.Image) (rankMatch, error)
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
	h := &rosterIngestHarness{
		Ingester: New(store, blobs, engine),
		t:        t,
		store:    store,
		blobs:    blobs,
		engine:   engine,
	}
	// Every roster test needs rank supplied somehow: rosterFrame draws
	// geometric cards, not real badge sprites (see its own doc comment on
	// why pixel realism is not this package's job to fake), so the real NCC
	// matcher would find nothing to match and route every frame to review.
	// Default to "R1" (newRosterIngestHarness's own default group) so a test
	// that builds a harness manually, scripts its own header OCR text, and
	// never calls newRosterIngestHarness at all (TestIngestRosterDiscards
	// TheOccludedTopRow) still gets a passing rank match without asking for
	// one explicitly.
	h.stubRankFor("R1")
	return h
}

// stubRankFor makes matchRankBadge return rank, at a score and gap nowhere
// near rankBadgeMinGap's floor, for every frame until the test ends — the
// same seam preprocess_options_test.go's spyPreprocess uses for OCR
// (visionPreprocess, roster.go). Idempotent: calling it again (as
// newRosterIngestHarness does once it knows the fixture's actual group)
// just swaps the active stub, and only the very first call registers the
// t.Cleanup that restores the real vision-based matcher.
func (h *rosterIngestHarness) stubRankFor(rank string) {
	h.t.Helper()
	if h.origMatchRankBadge == nil {
		h.origMatchRankBadge = matchRankBadge
		h.t.Cleanup(func() { matchRankBadge = h.origMatchRankBadge })
	}
	matchRankBadge = func(img image.Image) (rankMatch, error) {
		return rankMatch{Rank: rank, Score: 1.0, Gap: 1.0}, nil
	}
}

// stubRankError makes matchRankBadge fail for every frame, so the two
// branches IngestRoster takes on a rank failure can be exercised at all --
// newHarness's default stub always succeeds, which is why neither had a test
// before. The error is supplied by the caller rather than fixed here because
// the whole point of those branches is that they discriminate on it:
// ErrNoConfidentRank means this frame's badge was unreadable and routes to
// review, anything else means the embedded template set is broken and must
// fail the run (roster.go).
func (h *rosterIngestHarness) stubRankError(err error) {
	h.t.Helper()
	if h.origMatchRankBadge == nil {
		h.origMatchRankBadge = matchRankBadge
		h.t.Cleanup(func() { matchRankBadge = h.origMatchRankBadge })
	}
	matchRankBadge = func(img image.Image) (rankMatch, error) {
		return rankMatch{}, err
	}
}

// stubRankSequence makes matchRankBadge return ranks[i] on the i-th call,
// rather than stubRankFor's single constant rank -- needed to reproduce
// capture 1's documented group-key oscillation (docs/superpowers/specs/
// evidence/m4-ocr-2026-08-14, finding 10) directly instead of approximating
// it with one rank throughout. matchRankBadge is called exactly once per
// frame (IngestRoster's main loop), so len(ranks) must equal the number of
// list frames the test adds. A second call to stubRankSequence (as a re-run
// test needs, once per IngestRoster call) starts a fresh counter at 0 --
// each call installs its own closure over its own index.
func (h *rosterIngestHarness) stubRankSequence(ranks []string) {
	h.t.Helper()
	if h.origMatchRankBadge == nil {
		h.origMatchRankBadge = matchRankBadge
		h.t.Cleanup(func() { matchRankBadge = h.origMatchRankBadge })
	}
	i := 0
	matchRankBadge = func(img image.Image) (rankMatch, error) {
		if i >= len(ranks) {
			h.t.Fatalf("stubRankSequence: matchRankBadge called more times than the %d ranks scripted", len(ranks))
		}
		r := ranks[i]
		i++
		return rankMatch{Rank: r, Score: 1.0, Gap: 1.0}, nil
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
//
// One property this sizing removed rather than fixed: nothing in this
// package's fixture suite exercises a genuinely SPARSE region anymore (a
// real frame whose list content runs out with several rows of blank space
// still inside the scanned region). Round-2 review measured that curve
// directly, artificially truncating a real frame's content: contrast
// 91.2/78.0/65.0/51.7/38.7/25.5 for 6/5/4/3/2/1 real rows, 12.7 with none --
// so it is a real, present behavior (see phaseContrastFloor's comment for
// where the current floor sits on it), just not one any fixture here
// exercises. A future test wanting to cover it would need to keep imgH
// oversized deliberately rather than tight, which is exactly what this
// change moved away from for the (different, also real) reason above.
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
	// ambiguousName appends one row, "kaen445", scored against a registered
	// "kain445" -- one substitution in a 7-char name lands at 85, squarely
	// in the 75-92 review band.
	//
	// The read used to be "kaln445". That stopped being ambiguous when the
	// matcher learned OCR's confusable pairs: l/i is one of them, so the same
	// row now scores 97 and auto-accepts, which is the new scoring working
	// rather than a regression. The fixture moved to a substitution that is
	// genuinely a different character (i/e) so the test keeps asserting what
	// it was written to assert -- that a score in the review band is queued
	// rather than guessed -- instead of quietly becoming a test of the
	// confusable table.
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
	h.stubRankFor(group)

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
			name: "kaen445", power: "Power: 200.0M", level: "Lv.30", lastActive: "Online",
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

// distinctRowNames are twelve names with no shared root or numeric suffix,
// so no pair of them can accidentally land in roster.TokenSetRatio's
// ambiguous band the way "Row00" vs "Row01" (one digit apart) would —
// used wherever a test needs several rows that must each create a distinct
// member rather than match or ambiguously collide with one another.
var distinctRowNames = []string{
	"Zephyr", "Quokka", "Umbrella", "Xylophone", "Yonderland", "Cascade",
	"Thunderbolt", "Falconry", "Meadowlark", "Granitepeak", "Ripplewave",
	"Saltmarsh",
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

// rowResultsConf is rowResults with the name's OCR confidence under the
// test's control -- the numeric fields stay at 0.9 so nothing else in the row
// is what routes it.
func rowResultsConf(name string, nameConf float64) []ocr.Result {
	return []ocr.Result{
		{Text: name, Confidence: nameConf},
		{Text: "Power: 200.0M", Confidence: 0.9},
		{Text: "Lv.30", Confidence: 0.9},
		{Text: "Online", Confidence: 0.9},
	}
}

// reviewReasons counts the queued review rows by reason, for the assertions
// below that care which gate a row hit rather than only that it was queued.
func (h *rosterIngestHarness) reviewReasons() map[string]int {
	out := map[string]int{}
	for _, r := range h.store.Reviews {
		out[r.Reason]++
	}
	return out
}

// --- tests -----------------------------------------------------------------

// An empty name cannot identify anybody, so it must never become a member.
// Before this check, roster.Rank("") scored 0 against every member and the row
// fell through to the creation branch: capture 1 produced 20 empty-name rows
// per run and 122 ghost members across re-runs.
//
// The fixture is built so the *displacement* half fails loudly too, which is
// the more damaging consequence and the one an assertion on Created alone
// would miss. The group states a total of 1 and the frame carries two rows,
// the empty one first. If an empty name still consumes a slot of the group's
// creation budget, the real member behind it is refused as
// no_confident_match_group_full and Created stays 1 either way -- so the
// reason tally, not the count, is what distinguishes the fix from the bug.
func TestIngestRosterRefusesToCreateAMemberFromAnEmptyName(t *testing.T) {
	h := newHarness(t)
	h.addFrame(rosterFrame(3), 0)

	var results []ocr.Result
	results = append(results, ocr.Result{Text: "R1 Group 1/1", Confidence: 0.9})
	// An empty primary read is retried at raw line (see nameRetry), so this row
	// scripts the retry too — and scripts it empty, because the rule under test
	// is what happens when a name cannot be read AT ALL. Letting the retry
	// succeed here would test the retry instead.
	results = append(results, ocr.Result{Text: "", Confidence: 0.0})
	results = append(results, ocr.Result{Text: "", Confidence: 0.0})
	results = append(results, rowResultsConf("", 0.0)[1:]...)
	// Whitespace-only is NOT empty, so it does not retry and needs no extra
	// result — the distinction readFieldWithRetry draws, exercised here for
	// free. Confident as well as blank: the structural rule must not need the
	// confidence one.
	results = append(results, rowResultsConf("   ", 0.85)...)
	results = append(results, rowResultsConf("RealOne", 0.95)...)
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	if res.Created != 1 {
		t.Errorf("created %d members, want 1 (only RealOne)", res.Created)
	}
	for _, m := range h.store.members {
		if strings.TrimSpace(m.Name) == "" {
			t.Errorf("a member was created with an empty name (id %d, rank %q)", m.ID, m.Rank)
		}
	}

	reasons := h.reviewReasons()
	if reasons["unreadable_name"] != 2 {
		t.Errorf("unreadable_name reviews = %d, want 2 (the empty and the whitespace-only row); all reasons: %v", reasons["unreadable_name"], reasons)
	}
	if reasons["no_confident_match_group_full"] != 0 {
		t.Errorf("a real member was displaced into no_confident_match_group_full by an empty-name row consuming the group's creation budget; all reasons: %v", reasons)
	}
}

// The name is the one field whose MinConf is enforced rather than advisory,
// and it is enforced only on the creation branch -- see nameSpec's own doc
// comment. A name read too poorly to trust must not mint an identity that
// every later capture then tries to match against.
func TestIngestRosterRefusesToCreateAMemberFromALowConfidenceName(t *testing.T) {
	h := newHarness(t)
	h.addFrame(rosterFrame(2), 0)

	var results []ocr.Result
	results = append(results, ocr.Result{Text: "R1 Group 20/20", Confidence: 0.9})
	results = append(results, rowResultsConf("Grbldd", 0.2)...) // non-empty, matches nobody, below nameSpec.MinConf
	results = append(results, rowResultsConf("GoodName", 0.95)...)
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	if res.Created != 1 {
		t.Errorf("created %d members, want 1 (only GoodName)", res.Created)
	}
	for _, m := range h.store.members {
		if m.Name == "Grbldd" {
			t.Error("a member was created from a name read below nameSpec.MinConf")
		}
	}
	if reasons := h.reviewReasons(); reasons["low_confidence_name"] != 1 {
		t.Errorf("low_confidence_name reviews = %d, want 1; all reasons: %v", reasons["low_confidence_name"], reasons)
	}
}

// The asymmetry above is deliberate and this pins it. A fuzzy score of 92+
// against a member who already exists is much stronger evidence that a read is
// right than the OCR engine's own confidence is -- on capture 1, seven rows
// scored below nameSpec.MinConf and still auto-matched a real member. Applying
// the floor to matching as well as creation would throw those away for nothing.
func TestIngestRosterStillMatchesALowConfidenceNameAgainstAKnownMember(t *testing.T) {
	h := newHarness(t)
	h.store.nextMemberID++
	h.store.members = append(h.store.members, db.Member{
		ID: h.store.nextMemberID, AllianceID: 1, Name: "Lothar232",
		NameNormalized: roster.Normalize("Lothar232"), Rank: "R1", Active: true,
	})

	h.addFrame(rosterFrame(1), 0)
	var results []ocr.Result
	results = append(results, ocr.Result{Text: "R1 Group 20/20", Confidence: 0.9})
	results = append(results, rowResultsConf("Lothar232", 0.2)...) // well below MinConf, but an exact match
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Matched != 1 {
		t.Errorf("matched %d, want 1: a confident name match must survive a low OCR confidence", res.Matched)
	}
	if res.Created != 0 {
		t.Errorf("created %d, want 0", res.Created)
	}
	if reasons := h.reviewReasons(); reasons["low_confidence_name"] != 0 {
		t.Errorf("the creation-branch floor was applied to a matched row; all reasons: %v", reasons)
	}
	if len(h.store.Facts) == 0 {
		t.Error("no facts written for the matched row")
	}
}

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

// "Low confidence" here means the *fuzzy match* score, not the OCR
// confidence: the row's name reads cleanly and lands in roster.ReviewFloor's
// 75-92 band against a known member. The OCR-confidence gate on the name is a
// separate rule with its own tests
// (TestIngestRosterRefusesToCreateAMemberFromALowConfidenceName and the two
// beside it), and the two are deliberately independent — see nameSpec's doc
// comment.
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

// GroupTally.Parsed and GroupTally.MatchedOrCreated answer different
// questions, and this is the fixture where they diverge: eleven rows match a
// known member and a twelfth reads ambiguously and is queued. Parsed counts
// twelve because twelve bands reached OCR; MatchedOrCreated counts eleven
// because that is what the group actually yielded. The gap of one is the
// ambiguous row's NAME-class review -- not the review queue as a whole, which
// on a real capture also carries a row per unparseable or low-confidence
// numeric field on rows that resolved perfectly well.
//
// Asserted here rather than left to a gate: `control ingest` prints this as
// yielded=, and the roster gate needs Postgres, the blob store and tesseract,
// so nothing in `make test` would notice the exported counter silently going
// to zero.
func TestIngestRosterCountsMembersYieldedSeparatelyFromRowsParsed(t *testing.T) {
	h := newRosterIngestHarness(t, rosterFixture{
		group: "R2", groupTotal: 11, existing: 11, ambiguousName: true,
	})

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	got := res.PerGroup["R2"]
	if got.Parsed != 12 || got.MatchedOrCreated != 11 {
		t.Errorf("R2 tally = %+v, want parsed 12 and matched-or-created 11 (the ambiguous row is parsed and yields nobody)", got)
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

// A frame that scrolled a whole number of rows since the last one shows a
// whole new screenful, and every band on it is a row nothing has read.
//
// This test used to assert the opposite. It was called
// TestIngestRosterDiscardsTheOccludedTopRow and it pinned the drop of each
// continuing frame's topmost band, on the theory that the sticky header cut
// that row in half -- so two frames scrolled exactly six rows apart were
// expected to yield 11 members out of 12 rows, and the twelfth was scripted
// nowhere. The theory is false twice over: memberListRegion.Y1 sits below the
// sticky header's bottom edge, and SegmentRows never emits a bisected band in
// the first place, because collectBands only ever cuts at a confirmed
// inter-card gap (see the drop's own former site in roster.go). The row that
// assertion was throwing away is a real member.
//
// Six rows plus six rows is twelve, and the dedupe must not turn it into
// eleven (a dropped band) or thirteen (a band counted twice).
func TestIngestRosterCollectsEveryRowOfAFrameScrolledAWholeNumberOfRows(t *testing.T) {
	h := newHarness(t)

	frame1 := rosterFrame(6)
	frame2 := rosterFrame(6)
	h.addFrame(frame1, 0)   // first frame of the group
	h.addFrame(frame2, 672) // scrolled by exactly 6 rows * 112px pitch

	var results []ocr.Result
	results = append(results, ocr.Result{Text: "R1 Group 20/20", Confidence: 0.9})
	for k := 0; k < 6; k++ {
		results = append(results, rowResults(distinctRowNames[k])...)
	}
	results = append(results, ocr.Result{Text: "R1 Group 20/20", Confidence: 0.9})
	for k := 6; k < 12; k++ {
		results = append(results, rowResults(distinctRowNames[k])...)
	}
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if got := res.PerGroup["R1"].Parsed; got != 12 {
		t.Errorf("parsed %d rows across two frames, want 12 (6 + 6, none dropped and none counted twice)", got)
	}
	if res.Created != 12 {
		t.Errorf("created %d members, want 12", res.Created)
	}
}

// The status column has two states drawn differently: a grey elapsed time and
// "Online" in green with a dark outline. The shipped luma preprocessing parses
// 7 of capture 1's 24 Online bands and 250 of its 253 elapsed ones; a
// green-channel read of the same crop parses 12 of 24 Online and only 202 of
// 253 elapsed. Neither serves both, so the green channel is a RETRY on a read
// that did not parse -- never a replacement.
func TestIngestRosterRetriesAnUnparseableStatusOnTheGreenChannel(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R1")
	h.addFrame(rosterFrame(1), 0)

	h.engine.Results = []ocr.Result{
		{Text: groupHeaderText("R1", 5), Confidence: 0.9},
		{Text: "Zephyr", Confidence: 0.9},
		{Text: "Power: 200.0M", Confidence: 0.9},
		{Text: "Lv.30", Confidence: 0.9},
		// What luma makes of the outlined green wordmark.
		{Text: "oo", Confidence: 0.9},
		// The green channel's read of the same crop.
		{Text: "Online", Confidence: 0.9},
	}

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 1 {
		t.Fatalf("created %d members, want 1", res.Created)
	}
	if got := h.reviewReasons()["unparseable_last_active"]; got != 0 {
		t.Errorf("queued %d unparseable_last_active rows, want 0 -- the retry parsed it", got)
	}
	var found bool
	for _, f := range h.store.Facts {
		if f.Metric == "last_active_hours" {
			found = true
			if f.Value != 0 {
				t.Errorf("last_active_hours = %v, want 0 (Online)", f.Value)
			}
		}
	}
	if !found {
		t.Error("no last_active_hours fact written")
	}
}

// The other half: a retry that also fails must leave the PRIMARY's raw text on
// the review row. A number has no roster behind it, so the reviewer's only
// evidence is what the shipped read saw -- replacing it with the retry's
// output would hide the shipped path's own behaviour on exactly the crops it
// is failing.
func TestIngestRosterKeepsTheShippedStatusReadWhenTheGreenRetryAlsoFails(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R1")
	h.addFrame(rosterFrame(1), 0)

	h.engine.Results = []ocr.Result{
		{Text: groupHeaderText("R1", 5), Confidence: 0.9},
		{Text: "Zephyr", Confidence: 0.9},
		{Text: "Power: 200.0M", Confidence: 0.9},
		{Text: "Lv.30", Confidence: 0.9},
		{Text: "luma saw this", Confidence: 0.9},
		{Text: "green saw this", Confidence: 0.9},
	}

	if _, err := h.IngestRoster(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	var raw string
	for _, r := range h.store.Reviews {
		if r.Reason == "unparseable_last_active" {
			raw = r.RawText
		}
	}
	if raw != "luma saw this" {
		t.Errorf("review row raw text = %q, want the shipped read %q", raw, "luma saw this")
	}
}

// A roster capture photographs most rows three or more times. Until this
// test's fix exactly one of those photographs -- the first -- ever reached
// OCR, because the geometric dedupe compared a band's position against the
// last row collected and skipped anything already covered. A row whose first
// sighting read badly was therefore never read again, however clean the next
// photograph of it was.
//
// Capture 1 loses two members to exactly that and nothing else. Bujangann
// reads at confidence 0.03 on seq 7 and 0.00 on seq 8, then "Bujangann" at
// 0.80 on seq 9; Riesige Banane reads at 0.38 on seq 6 -- just under
// nameSpec.MinConf -- and 0.77 on seq 7. Both good reads were photographed
// and thrown away.
func TestIngestRosterRereadsARowItsFirstSightingCouldNotResolve(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R1")

	h.addFrame(rosterFrame(1), 0)
	// A small offset: the same physical row, a little further up the screen.
	// Well inside half a pitch, so the dedupe recognizes it as the row it
	// already collected rather than a new one.
	h.addFrame(rosterFrame(1), 20)

	h.engine.Results = []ocr.Result{
		{Text: groupHeaderText("R1", 5), Confidence: 0.9},
		// Read correctly and far below nameSpec.MinConf, so processRow
		// refuses to create a member from it. This is Riesige Banane's shape.
		{Text: "Zephyr", Confidence: 0.1},
		{Text: "Power: 200.0M", Confidence: 0.9},
		{Text: "Lv.30", Confidence: 0.9},
		{Text: "Online", Confidence: 0.9},
		{Text: groupHeaderText("R1", 5), Confidence: 0.9},
		{Text: "Zephyr", Confidence: 0.9},
		{Text: "Power: 200.0M", Confidence: 0.9},
		{Text: "Lv.30", Confidence: 0.9},
		{Text: "Online", Confidence: 0.9},
	}

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 1 {
		t.Errorf("created %d members, want 1: the second photograph of a row nobody could read must be read", res.Created)
	}
	if reasons := h.reviewReasons(); reasons["low_confidence_name"] != 1 {
		t.Errorf("queued %d low_confidence_name rows, want 1 -- the first sighting still queues", reasons["low_confidence_name"])
	}
	// Parsed asks whether the scroll photographed the whole group. Re-reading
	// one row does not make the group larger, so it must stay 1.
	if got := res.PerGroup["R1"].Parsed; got != 1 {
		t.Errorf("parsed = %d, want 1: a re-read of a row already collected is not a second row", got)
	}
}

// A row nothing can read is re-read on every photograph of it, and each of
// those failures used to put another row in front of a human describing the
// same piece of work. Capture 1 photographs some rows seventeen times; the
// review queue went 271 -> 378 rows before this was suppressed.
func TestIngestRosterQueuesOneReviewRowPerUnresolvableRowNotPerPhotograph(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R1")

	for range 3 {
		h.addFrame(rosterFrame(1), 20)
	}

	var results []ocr.Result
	for range 3 {
		results = append(results, ocr.Result{Text: groupHeaderText("R1", 5), Confidence: 0.9})
		results = append(results, rowResultsConf("Zephyr", 0.1)...)
	}
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 0 {
		t.Fatalf("created %d members, want 0 -- every sighting is below nameSpec.MinConf", res.Created)
	}
	if got := h.reviewReasons()["low_confidence_name"]; got != 1 {
		t.Errorf("queued %d low_confidence_name rows, want 1: three photographs of one unreadable row are one piece of human work", got)
	}
}

// The other half of the same rule, and the one that stops a member being
// minted twice: a row that DID resolve is never read again. The second frame
// scripts a header and nothing else, so any field read on it exhausts the
// fake engine and fails the test by name.
func TestIngestRosterNeverRereadsARowThatAlreadyResolved(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R1")

	h.addFrame(rosterFrame(1), 0)
	h.addFrame(rosterFrame(1), 20)

	results := []ocr.Result{{Text: groupHeaderText("R1", 5), Confidence: 0.9}}
	results = append(results, rowResults("Zephyr")...)
	results = append(results, ocr.Result{Text: groupHeaderText("R1", 5), Confidence: 0.9})
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v (an OCR call past the second frame's header means a resolved row was read again)", err)
	}
	if res.Created != 1 {
		t.Errorf("created %d members, want 1", res.Created)
	}
	if res.Matched != 0 {
		t.Errorf("matched %d rows, want 0: the resolved row must not reach the matcher a second time", res.Matched)
	}
}

// TestIngestRosterQueuesAnUnreadableBadgeWithItsFullHeaderText is task 24's
// brief test 6, which was specified and never written. Two things are being
// pinned, and the second is the one a human depends on.
//
// First: a frame whose rank badge matched nothing confidently must not have
// its rows attributed to a guessed group. Rank is not supplied by
// roster_capture (see roster.go's package doc) so there is no fallback to
// take, and acting on an unidentified screen is what CLAUDE.md invariant #3
// forbids -- the whole frame goes to review and none of its rows are parsed.
//
// Second: the review row carries the header's *full* raw text. A reviewer
// opening this row needs to see what was actually read -- "{R3) Footloose
// 10/64 yi]" tells them the OCR side resolved the count fine and only the
// badge failed, which is a different fix from a header that came back as
// noise. A row that said only "unmatched_rank_badge" would send them to the
// screenshot to re-derive what the pipeline already knew.
func TestIngestRosterQueuesAnUnreadableBadgeWithItsFullHeaderText(t *testing.T) {
	h := newHarness(t)
	h.stubRankError(fmt.Errorf("badge scored 0.71 against 0.70: %w", ErrNoConfidentRank))

	h.addFrame(rosterFrame(6), 0)

	const headerText = "{R3) Footloose 10/64 yi]"
	// Only the header is scripted. If the frame's rows were OCR'd anyway --
	// the failure this test exists to catch -- FakeEngine would run out of
	// results and IngestRoster would error rather than silently pass.
	h.engine.Results = []ocr.Result{{Text: headerText, Confidence: 0.9}}

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 0 || res.Matched != 0 {
		t.Errorf("created %d / matched %d, want 0 / 0: no row may be attributed to a guessed rank group", res.Created, res.Matched)
	}

	var found *db.ReviewItem
	for k := range h.store.Reviews {
		if h.store.Reviews[k].Reason == "unmatched_rank_badge" {
			found = &h.store.Reviews[k]
			break
		}
	}
	if found == nil {
		t.Fatalf("no unmatched_rank_badge review row queued; got %d reviews", len(h.store.Reviews))
	}
	if found.RawText != headerText {
		t.Errorf("review RawText = %q, want the full header text %q", found.RawText, headerText)
	}
	if found.ScreenshotID == 0 {
		t.Error("review row has no screenshot reference; a reviewer cannot see the pixels it came from")
	}
}

// TestIngestRosterFailsRatherThanQueueingABrokenTemplateSet pins the
// discrimination the branch above depends on. loadRankTemplates' doc comment
// already says an error from it "means the binary itself is broken, not that
// one frame's rank is unreadable", and sync.Once makes that permanent for the
// process -- but roster.go used to route *any* rank error to a review row, so
// a build with a corrupt embedded template would have ingested a whole capture
// into the review queue and reported status "partial". That is a capture-sized
// pile of human work standing in for one build failure.
//
// The error deliberately does not wrap ErrNoConfidentRank, which is the only
// thing separating the two cases.
func TestIngestRosterFailsRatherThanQueueingABrokenTemplateSet(t *testing.T) {
	h := newHarness(t)
	brokenEmbed := errors.New("ingest: decoding embedded rank template rankbadges/r3.png: invalid PNG header")
	h.stubRankError(brokenEmbed)

	h.addFrame(rosterFrame(6), 0)
	h.engine.Results = []ocr.Result{{Text: "{R3) Footloose 10/64 yi]", Confidence: 0.9}}

	_, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err == nil {
		t.Fatal("IngestRoster returned no error on a broken template set, want the run to fail")
	}
	if !errors.Is(err, brokenEmbed) {
		t.Errorf("IngestRoster error = %v, want it to wrap the template-load failure", err)
	}
	for _, r := range h.store.Reviews {
		if r.Reason == "unmatched_rank_badge" {
			t.Error("a broken template set queued an unmatched_rank_badge review row; it must fail the run instead")
		}
	}
}

// TestIngestRosterQueuesEveryRowWhenTheGroupCountIsUnreadable is the ratio
// this task exists to fix. IngestRoster used to `continue` on a header-parse
// failure, so ONE unreadable field discarded EVERY row on the frame: capture
// 1's 21 R2 frames left 21 unparseable_group_header rows behind and nothing
// else, and the members on them vanished with no record that a row had been
// seen at all. That is the silent drop this milestone exists to prevent,
// committed by the ingest path itself -- and it would be wrong even if every
// count in that capture read perfectly, because one smudged header on a
// future capture loses a whole group the same way.
//
// The count and the rank are independent reads with independent failure
// paths (see IngestRoster's own comment): the badge here matches at full
// confidence, so the frame's rows have an honest rank to attach to and
// nothing about invariant #3 is being bent. What the missing count costs is
// the creation budget, not the frame.
//
// The assertion is on the PER-ROW count, not on "something was queued": one
// row per frame and one row per member both leave a non-empty review queue,
// and only the ratio distinguishes the defect from the fix.
func TestIngestRosterQueuesEveryRowWhenTheGroupCountIsUnreadable(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R2")
	h.addFrame(rosterFrame(3), 0)

	// Real text: capture 1's R2 headers read as the group's name with no
	// count-shaped token at all, on all 21 frames, at every geometry and
	// preprocessing shape measured (task 6's report; groupHeaderRegion's doc
	// comment). parseGroupHeader refuses it rather than guessing.
	results := []ocr.Result{{Text: "{R2) I'm Alright", Confidence: 0.9}}
	for _, name := range distinctRowNames[:3] {
		results = append(results, rowResults(name)...)
	}
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	reasons := h.reviewReasons()
	if reasons["unparseable_group_header"] != 1 {
		t.Errorf("queued %d unparseable_group_header rows, want 1: the header failure is still worth recording",
			reasons["unparseable_group_header"])
	}
	if got := reasons["no_confident_match_group_count_unknown"]; got != 3 {
		t.Errorf("queued %d per-row reviews for a countless group, want 3 -- one per row on the frame, not one for the frame; all reasons: %v", got, reasons)
	}

	// No budget means no creations, and this is the line not to cross: the
	// count is the structural guard against minting phantoms (design doc §4),
	// so a group whose size is unknown has none to spend. Ending the silent
	// drop must not become a licence to invent people.
	if res.Created != 0 {
		t.Errorf("created %d members for a group with no readable count, want 0: the count is what gates creation", res.Created)
	}

	tally, seen := res.PerGroup["R2"]
	if !seen {
		t.Fatalf("group R2 produced no tally at all; its badge matched, so the rows it carried are describable: %v", res.PerGroup)
	}
	if tally.Parsed != 3 {
		t.Errorf("tally parsed=%d, want 3: every band on the frame was segmented and read", tally.Parsed)
	}
	if tally.ExpectedKnown {
		t.Error("tally reports ExpectedKnown=true for a header that never parsed")
	}
	if tally.Expected != 0 {
		t.Errorf("tally expected=%d for an unread count, want 0 alongside ExpectedKnown=false", tally.Expected)
	}
	if res.Status != "partial" {
		t.Errorf("status = %q, want %q: a group whose size is unknown cannot be reconciled, so the capture is not complete", res.Status, "partial")
	}
}

// A countless group is not an inert group: it still matches its rows against
// members already known, which is the whole difference between "no creation
// budget" and the old "discard the frame". Capture 1's R2 members do not
// exist in `members` today, but on any later capture -- one where the header
// read, or after review resolves them -- they will, and every subsequent
// unreadable header must keep collecting their facts rather than re-queueing
// them forever.
func TestIngestRosterStillMatchesKnownMembersWhenTheGroupCountIsUnreadable(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R2")
	h.store.nextMemberID++
	known := h.store.nextMemberID
	h.store.members = append(h.store.members, db.Member{
		ID: known, AllianceID: 1, Name: "Zephyr",
		NameNormalized: roster.Normalize("Zephyr"), Rank: "R2", Active: true,
	})
	h.addFrame(rosterFrame(2), 0)

	results := []ocr.Result{{Text: "{R2) I'm Alright", Confidence: 0.9}}
	results = append(results, rowResults("Zephyr")...)
	results = append(results, rowResults("Quokka")...)
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Matched != 1 {
		t.Errorf("matched %d rows, want 1: a missing count withholds the creation budget, not the matcher", res.Matched)
	}
	if res.Created != 0 {
		t.Errorf("created %d members, want 0", res.Created)
	}
	if reasons := h.reviewReasons(); reasons["no_confident_match_group_count_unknown"] != 1 {
		t.Errorf("queued %d unmatched rows, want 1 (the row that matched nobody); all reasons: %v",
			reasons["no_confident_match_group_count_unknown"], reasons)
	}
	var wroteFor int64
	for _, f := range h.store.Facts {
		wroteFor = f.MemberID
	}
	if wroteFor != known {
		t.Errorf("facts written for member %d, want the known member %d: a matched row on a countless frame still carries its numbers", wroteFor, known)
	}
}

// A group whose count never parsed must not be reported `complete`, and the
// case that says so is the one where it also parsed no rows -- a collapsed
// group's frame, which carries a header and no list. Expected and Parsed are
// then both 0, so the arithmetic half of the reconciliation rule
// (Parsed != Expected) is satisfied and would call the capture complete: a
// claim that a group of unknown size was fully seen. Only ExpectedKnown
// distinguishes it, which is why the rule states the unknown case explicitly
// rather than leaning on a zero.
func TestIngestRosterRefusesToCallACountlessGroupComplete(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R2")
	h.addFrame(rosterFrame(0), 0)
	h.engine.Results = []ocr.Result{{Text: "{R2) I'm Alright", Confidence: 0.9}}

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	tally := res.PerGroup["R2"]
	if tally.Parsed != 0 || tally.Expected != 0 {
		t.Fatalf("tally parsed=%d expected=%d, want 0/0 -- this test is only meaningful when the arithmetic agrees", tally.Parsed, tally.Expected)
	}
	if res.Status != "partial" {
		t.Errorf("status = %q for a group whose size was never read, want %q", res.Status, "partial")
	}
}

// The budget is the GROUP's, not the frame's: once any frame of a group has
// read its count, a later frame whose header fails still creates. Capture 1
// cannot tell the two readings apart -- all 21 of its R2 headers fail, so
// "this frame has no count" and "this group has no count" coincide exactly on
// the only real capture there is, which is the condition under which the
// wrong one ships.
func TestIngestRosterKeepsTheBudgetAFrameOfItsGroupAlreadyRead(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R2")
	h.addFrame(rosterFrame(1), 0)
	// One card on the second frame too, three rows further down the list, so
	// exactly one new row is collected from it. (It used to draw two cards
	// and script one row, because IngestRoster discarded every continuing
	// frame's topmost band; it no longer does -- see the removed drop's site
	// in roster.go.)
	h.addFrame(rosterFrame(1), memberRowPitch*3)

	results := []ocr.Result{{Text: "{R2) I'm Alright 2/11", Confidence: 0.9}}
	results = append(results, rowResults("Zephyr")...)
	results = append(results, ocr.Result{Text: "{R2) I'm Alright", Confidence: 0.9})
	results = append(results, rowResults("Quokka")...)
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 2 {
		t.Errorf("created %d members, want 2: a header that failed on the SECOND frame cannot take away a budget the first frame read", res.Created)
	}
	reasons := h.reviewReasons()
	if reasons["unparseable_group_header"] != 1 {
		t.Errorf("queued %d unparseable_group_header rows, want 1", reasons["unparseable_group_header"])
	}
	if reasons["no_confident_match_group_count_unknown"] != 0 {
		t.Errorf("queued %d rows as countless, want 0: this group's count was read; all reasons: %v",
			reasons["no_confident_match_group_count_unknown"], reasons)
	}
	if tally := res.PerGroup["R2"]; !tally.ExpectedKnown || tally.Expected != 11 {
		t.Errorf("tally expected=%d known=%v, want 11/true", tally.Expected, tally.ExpectedKnown)
	}
}

// A failed-header frame is no longer invisible to the FRAME-ADJACENCY chain,
// and that is a consequence of removing the `continue` rather than a thing
// this task set out to change. The skipped branch sat ahead of gt.advance and
// of the prevGroupKey bookkeeping, so a dropped frame used to leave the frame
// AFTER it looking consecutive with the last frame actually processed. It no
// longer does: a group-A frame between two group-B frames now breaks B's
// chain, exactly as a readable group-A frame between them always has.
//
// The new behaviour is the one advance's own doc argues for -- the pixels
// moved across a group boundary are not evidence about either group's list,
// so contentY stays where the tracker left it and the top band is not
// skipped. What that costs is stated here rather than left to be
// rediscovered: rows the resumed cursor cannot distinguish from ones already
// collected are DEDUPED AWAY, which is an under-collection, and
// under-collection is a silent drop. The alternative is worse in a way this
// route cannot recover from -- attributing a cross-group scroll to B's list
// makes rows look new that are not, and creation is first-writer-wins -- so
// the trade is deliberate.
//
// On capture 1 it cost nothing (the yield arithmetic across the change is
// unchanged), because its group boundaries happen to fall where the frames
// either side are the same group. This test is the case that capture does
// not contain.
func TestIngestRosterBreaksTheFrameChainAcrossAFailedHeaderFrame(t *testing.T) {
	h := newHarness(t)
	h.stubRankSequence([]string{"R3", "R2", "R3"})
	h.addFrame(rosterFrame(2), 0)
	h.addFrame(rosterFrame(1), memberRowPitch)
	h.addFrame(rosterFrame(2), memberRowPitch)

	results := []ocr.Result{{Text: "[R3) Footloose 10/64", Confidence: 0.9}}
	results = append(results, rowResults(distinctRowNames[0])...)
	results = append(results, rowResults(distinctRowNames[1])...)
	results = append(results, ocr.Result{Text: "{R2) I'm Alright", Confidence: 0.9})
	results = append(results, rowResults(distinctRowNames[2])...)
	results = append(results, ocr.Result{Text: "[R3) Footloose 10/64", Confidence: 0.9})
	// Scripted but expected to go unread. If a future change restores the old
	// chain semantics, R3's third frame collects a row and these results are
	// consumed -- which the tally assertion below names, rather than letting
	// the engine run out somewhere unrelated.
	results = append(results, rowResults(distinctRowNames[3])...)
	results = append(results, rowResults(distinctRowNames[4])...)
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if got := res.PerGroup["R3"].Parsed; got != 2 {
		t.Errorf("R3 parsed %d rows across two frames, want 2: the R2 frame between them breaks R3's chain, so R3's cursor does not advance and its second frame's bands read as already collected", got)
	}
	if res.Created != 2 {
		t.Errorf("created %d members, want 2: R3's two rows, and none for the countless R2 frame", res.Created)
	}
	// The under-collection must never become a DUPLICATION: the same member
	// minted twice is the roster route's unrecoverable failure (design §4,
	// condition 2), and it is what advancing a resumed group's cursor by a
	// cross-group offset would risk.
	names := map[string]int{}
	for _, m := range h.store.members {
		names[m.Name]++
		if names[m.Name] > 1 {
			t.Errorf("member %q was created twice across the interleaved frames", m.Name)
		}
	}
	if reasons := h.reviewReasons(); reasons["no_confident_match_group_count_unknown"] != 1 {
		t.Errorf("queued %d countless rows, want 1 (the R2 frame's own row); all reasons: %v",
			reasons["no_confident_match_group_count_unknown"], reasons)
	}
}

// A count that arrives on a LATER frame of the same group is still the
// count. groupTracker is created on the group's first frame, so a group
// whose first header failed and whose second read cleanly would otherwise
// stay budgetless for the rest of the capture -- the evidence for its size
// having been seen and thrown away. Order is not something the capture
// controls: which frame a group is first sighted on depends on where a
// swipe happened to land.
func TestIngestRosterAdoptsACountThatOnlyLaterFramesCouldRead(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R2")
	h.addFrame(rosterFrame(1), 0)
	// One card on the second frame too, three rows further down the list, so
	// exactly one new row is collected from it.
	h.addFrame(rosterFrame(1), memberRowPitch*3)

	results := []ocr.Result{{Text: "{R2) I'm Alright", Confidence: 0.9}}
	results = append(results, rowResults("Zephyr")...)
	results = append(results, ocr.Result{Text: "{R2) I'm Alright 2/11", Confidence: 0.9})
	results = append(results, rowResults("Quokka")...)
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 1 {
		t.Errorf("created %d members, want 1: the second frame's header supplied the budget the first frame's did not", res.Created)
	}
	tally := res.PerGroup["R2"]
	if !tally.ExpectedKnown || tally.Expected != 11 {
		t.Errorf("tally expected=%d known=%v, want 11/true once a frame read the header", tally.Expected, tally.ExpectedKnown)
	}
	if reasons := h.reviewReasons(); reasons["no_confident_match_group_count_unknown"] != 1 {
		t.Errorf("queued %d rows for the countless first frame, want 1; all reasons: %v",
			reasons["no_confident_match_group_count_unknown"], reasons)
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

// --- task 27: ingest must survive a capture whose groups interleave -------

// TestGroupTrackerAdvanceResumesAnInterruptedGroupRatherThanRestarting is the
// regression test for groupTracker.advance's actual arithmetic, decoupled
// from image segmentation entirely -- it drives the exact header sequence
// capture 1 measured (docs/superpowers/specs/evidence/m4-ocr-2026-08-14,
// finding 10: "R4 R3 R3 ... R3 R2 R2 R2 R3 R3 R3 R2 R2 R3 R3 R3 R2 R2 R2 R3
// R3 ...", excerpted here to its shortest oscillating shape) through two
// independent trackers and asserts the exact contentY each call returns.
//
// Before task 27's fix, the third R3 sighting (index 3, a return after one
// R2 frame) would have reported contentY=0 -- the bug this test exists to
// catch, per its own name: a group returned to after an interruption must
// resume, not restart.
func TestGroupTrackerAdvanceResumesAnInterruptedGroupRatherThanRestarting(t *testing.T) {
	type step struct {
		group        string
		offsetPx     int
		wantContentY int
	}
	// R3, R3, R2, R3, R2, R2 -- finding 10's own oscillation, at its
	// shortest. offsetPx is only ever added when the immediately preceding
	// frame carried the SAME group's header (see advance's own doc comment
	// for why); every other value here is deliberately unreachable-looking
	// (999) to prove it is ignored on a group switch, not just unused by
	// coincidence of a convenient number.
	steps := []step{
		{group: "R3", offsetPx: 0, wantContentY: 0},     // R3's first-ever frame
		{group: "R3", offsetPx: 112, wantContentY: 112}, // continuing: accumulate
		{group: "R2", offsetPx: 999, wantContentY: 0},   // R2's first-ever frame
		{group: "R3", offsetPx: 999, wantContentY: 112}, // returning: RESUME 112, not reset to 0
		{group: "R2", offsetPx: 999, wantContentY: 0},   // returning: resume R2's own leftover (0)
		{group: "R2", offsetPx: 112, wantContentY: 112}, // continuing: accumulate again
	}

	groups := map[string]*groupTracker{}
	var prevGroup string
	var havePrev bool
	for idx, s := range steps {
		gt, ok := groups[s.group]
		if !ok {
			gt = &groupTracker{}
			groups[s.group] = gt
		}
		sameAsPrev := havePrev && s.group == prevGroup
		if gotContentY := gt.advance(s.offsetPx, sameAsPrev); gotContentY != s.wantContentY {
			t.Errorf("step %d (%s): contentY = %d, want %d", idx, s.group, gotContentY, s.wantContentY)
		}
		prevGroup, havePrev = s.group, true
	}
}

// groupHeaderText builds the header OCR text newRosterIngestHarness's own
// fixture builder would, for tests below that script IngestRoster's frames
// by hand rather than through rosterFixture.
func groupHeaderText(group string, total int) string {
	return fmt.Sprintf("%s Group %d/%d", group, total, total)
}

// TestIngestRosterSurvivesInterleavedGroupsAcrossARerun is task 27's main
// regression test, built directly from the sequence capture 1 measured
// (finding 10): R3, R3, R2, R3, R2, R2. Frames 4 and 5 are R3 and R2 each
// returning after the other interleaved -- under the resumed-offset fix
// (TestGroupTrackerAdvanceResumesAnInterruptedGroupRatherThanRestarting)
// their one row apiece lands on ground the group's own earlier frame already
// covered, so geometric dedupe correctly recognizes "nothing new here" and
// neither contributes a row. That first pass is not, by itself, a
// regression test: reset-to-zero and resume-to-leftover computed the same
// "already covered" verdict for this particular fixture's small offsets,
// so a single run does not distinguish the fixed code from the code before
// it (see the task 27 report for why a single run does not reliably
// distinguish them here, and what a real capture's larger offsets do
// instead).
//
// What isolates the actual bug is calling IngestRoster a SECOND time over
// the identical capture -- exactly what `control ingest --capture 1`
// running again does, whether because an earlier attempt crashed partway
// (task 27's brief: "the database already holds facts and review rows from
// the aborted runs") or a human simply re-ran it. Every frame's group header
// and every row's OCR text is scripted identically both times, so the second
// run is a genuine re-observation of the same four members within the same
// capture -- and participation_facts' own unique key (member_id, metric,
// period_key, source, observed_at) makes that the SAME fact both times,
// because observed_at is pinned to the capture's started_at for every frame
// in it, not to wall-clock time (see IngestRoster's package doc). Before
// this task's fix, writeFacts called the plain, always-insert InsertFact, so
// the second run doubled every fact already on file rather than recognizing
// it — which is what this test's fact-count assertion below caught, run
// against the code before this change: the second run's fact count was
// exactly double the first's, not equal to it.
func TestIngestRosterSurvivesInterleavedGroupsAcrossARerun(t *testing.T) {
	h := newHarness(t)

	// Frame 1: R3's first-ever frame. One row, Zephyr.
	h.addFrame(rosterFrame(1), 0)
	// Frame 2: R3 continuing, one row further down the list. One row, Quokka.
	h.addFrame(rosterFrame(1), memberRowPitch)
	// Frame 3: R2's own first-ever frame, independent of R3's accounting.
	h.addFrame(rosterFrame(1), 0)
	// Frame 4: R3 returning after R2 interleaves. See the test's own doc
	// comment above: this row lands on ground frame 2 already covered, so
	// it contributes nothing -- correctly, not as a bug.
	h.addFrame(rosterFrame(1), 999)
	// Frame 5: R2 returning after R3. Same shape as frame 4, for R2.
	h.addFrame(rosterFrame(1), 999)
	// Frame 6: R2 continuing, one row further down. One row, Foxtrot.
	h.addFrame(rosterFrame(1), memberRowPitch)

	scriptOneIngest := func() []ocr.Result {
		var results []ocr.Result
		results = append(results, ocr.Result{Text: groupHeaderText("R3", 20), Confidence: 0.9})
		results = append(results, rowResults("Zephyr")...)
		results = append(results, ocr.Result{Text: groupHeaderText("R3", 20), Confidence: 0.9})
		results = append(results, rowResults("Quokka")...)
		results = append(results, ocr.Result{Text: groupHeaderText("R2", 20), Confidence: 0.9})
		results = append(results, rowResults("Umbrella")...)
		results = append(results, ocr.Result{Text: groupHeaderText("R3", 20), Confidence: 0.9})
		// frame 4: no rows survive geometric dedupe, so no field reads to script.
		results = append(results, ocr.Result{Text: groupHeaderText("R2", 20), Confidence: 0.9})
		// frame 5: likewise.
		results = append(results, ocr.Result{Text: groupHeaderText("R2", 20), Confidence: 0.9})
		results = append(results, rowResults("Foxtrot")...)
		return results
	}

	h.stubRankSequence([]string{"R3", "R3", "R2", "R3", "R2", "R2"})
	h.engine.Results = scriptOneIngest()

	res1, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("first IngestRoster: %v", err)
	}
	if res1.Created != 4 {
		t.Fatalf("first run created %d members, want 4 (Zephyr, Quokka, Umbrella, Foxtrot)", res1.Created)
	}
	factsAfterFirstRun := len(h.store.Facts)
	if factsAfterFirstRun == 0 {
		t.Fatal("first run wrote no facts at all -- test setup is broken")
	}

	// Re-run against the identical capture: same frames (CaptureFrames
	// returns the same slice both times), same screenshots, same header and
	// row text scripted again.
	h.stubRankSequence([]string{"R3", "R3", "R2", "R3", "R2", "R2"})
	h.engine.Results = append(h.engine.Results, scriptOneIngest()...)

	res2, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("second IngestRoster (re-run over the same capture): %v", err)
	}
	if res2.Created != 0 {
		t.Errorf("second run created %d members, want 0 -- all four already exist from the first run", res2.Created)
	}
	if res2.Matched != 4 {
		t.Errorf("second run matched %d rows, want 4 -- the same four members, re-observed", res2.Matched)
	}
	if got := len(h.store.Facts); got != factsAfterFirstRun {
		t.Errorf("facts after the re-run = %d, want %d unchanged -- a member re-observed within the same capture must not double its fact count", got, factsAfterFirstRun)
	}
}

// TestIngestRosterUpsertsARepeatObservationRatherThanDuplicatingTheFact is
// the within-a-single-run twin of the rerun test above: the same member's
// row can legitimately appear twice within ONE IngestRoster call too, not
// just across separate runs -- capture 1's interleaving is one cause, but an
// ordinary overlap between two screenfuls of a group that never closed is
// another, and needs no group switch to demonstrate. The two rows here are
// deliberately far enough apart geometrically that the group's row-position dedupe does
// NOT recognize them as the same physical row -- only name-identity does,
// which is the case this task's fix (writeFacts calling UpsertFact) has to
// carry once geometric dedupe has already let a genuine repeat through.
//
// The decision under test: a repeat observation within the same capture
// upserts rather than duplicating the fact, keeping whichever read carries
// the higher confidence -- justified in UpsertFact's own doc comment
// (internal/db/analytics.go) against CLAUDE.md's append-only invariant and
// the "identical screenshot bytes still earn a row" precedent it is
// deliberately NOT following here (that precedent is about distinct capture
// *events*; this is one OCR engine reading one already-covered instant
// twice within a single capture, which participation_facts' own key treats
// as one fact by construction).
func TestIngestRosterUpsertsARepeatObservationRatherThanDuplicatingTheFact(t *testing.T) {
	h := newHarness(t)
	h.stubRankFor("R1")

	h.addFrame(rosterFrame(1), 0)
	// A huge offset, not a realistic scroll distance: the point is to place
	// this frame's one real row far past every collected row's dedupe window, so it
	// reaches processRow as a "new" row on identity grounds even though it
	// names the same member as frame 1's row.
	h.addFrame(rosterFrame(1), 5000)

	results := []ocr.Result{
		{Text: groupHeaderText("R1", 5), Confidence: 0.9},
		{Text: "Kilo", Confidence: 0.9},
		{Text: "Power: 200.0M", Confidence: 0.85},
		{Text: "Lv.30", Confidence: 0.85},
		{Text: "Online", Confidence: 0.85},
		{Text: groupHeaderText("R1", 5), Confidence: 0.9},
		// The exact same row, read again -- a cleaner second pass at the
		// identical figures, which is what a repeat observation of
		// unchanged game state within one capture looks like.
		{Text: "Kilo", Confidence: 0.9},
		{Text: "Power: 200.0M", Confidence: 0.95},
		{Text: "Lv.30", Confidence: 0.95},
		{Text: "Online", Confidence: 0.95},
	}
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 1 {
		t.Fatalf("created %d members, want 1 -- Kilo, once", res.Created)
	}
	if res.Matched != 1 {
		t.Errorf("matched %d rows, want 1 -- Kilo's second row, matching itself", res.Matched)
	}
	if got := len(h.store.Facts); got != 3 {
		t.Fatalf("facts written = %d, want exactly 3 (power, level, last_active_hours) -- a repeat observation is the same fact, not a second one", got)
	}
	for _, f := range h.store.Facts {
		if f.Confidence != 0.95 {
			t.Errorf("fact %+v confidence = %v, want 0.95 -- the second, cleaner read of the same figure should have won", f, f.Confidence)
		}
	}
}

// A roster name crop that PSM 7's layout analysis refuses to look at reads
// empty, and an empty read is not a near miss -- no threshold and no fuzzy
// match reaches it. The VS name field has retried such crops at raw line since
// the layout blindness was measured; the roster name field was left calling
// readField directly, so the same defect went unhandled on the route that
// creates the members every VS row is later matched against.
//
// Measured on capture 1 with `make probe-roster PROBE_ARGS=-roster.retry`:
// 87 of 331 row bands read empty at PSM 7 and every one of them reads at
// PSM 13, taking exact name reads from 141 to 147.
//
// This is safe here for the reason CLAUDE.md gives for names and withholds
// from points: a name has a known roster behind it, so a poor retried read
// simply fails to match, while a number has no such guard and a raw-line
// retry can manufacture a plausible value.
func TestIngestRosterRetriesANameReadTheLayoutAnalysisRefused(t *testing.T) {
	h := newHarness(t)
	h.addFrame(rosterFrame(1), 0)

	var results []ocr.Result
	results = append(results, ocr.Result{Text: "R1 Group 1/1", Confidence: 0.9})
	// The primary read returns nothing; the retry is the second result, and
	// the remaining fields follow it. Without the retry the engine hands
	// "Lothar232" to the POWER field and the name stays empty.
	results = append(results, ocr.Result{Text: "", Confidence: 0.0})
	results = append(results, ocr.Result{Text: "Lothar232", Confidence: 0.9})
	results = append(results, ocr.Result{Text: "Power: 200.0M", Confidence: 0.9})
	results = append(results, ocr.Result{Text: "Lv.30", Confidence: 0.9})
	results = append(results, ocr.Result{Text: "Online", Confidence: 0.9})
	h.engine.Results = results

	res, err := h.IngestRoster(context.Background(), 1, testPeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	if res.Created != 1 {
		t.Fatalf("created %d members, want 1: the empty primary read was not retried", res.Created)
	}
	var names []string
	for _, m := range h.store.members {
		names = append(names, m.Name)
	}
	if len(names) != 1 || names[0] != "Lothar232" {
		t.Errorf("members = %q, want exactly [Lothar232] from the retried read", names)
	}
	if n := h.reviewReasons()["unreadable_name"]; n != 0 {
		t.Errorf("unreadable_name reviews = %d, want 0: the retry read the name, so nothing should have queued", n)
	}
}
