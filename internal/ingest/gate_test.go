//go:build m4gate

// The M4 gate: ingest reproduces a hand-checked VS ranking capture.
//
// Behind a build tag because, unlike `make test`, it needs three things at
// once — Postgres (via internal/dbtest, never the dev database), the blob
// store holding the capture's frames, and the tesseract binary. Run it with
// `make gate-m4`.
//
// It asserts the three conditions from the design doc's §7, and the second is
// what makes this a gate rather than an accuracy score: a pipeline that
// silently drops the rows it finds hard scores well on condition 1 alone, and
// silent dropping is the failure this milestone exists to prevent.
//
//  1. parsed value within 1% of the hand-checked value on >= 95% of rows;
//  2. every discrepancy produced a review_queue row -- none dropped;
//  3. the capture still reconciles to 'complete' after ingest.
package ingest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/dbtest"
	"github.com/tomharris/lw-manager/internal/ingest"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
)

const (
	// gateRowAccuracy and gateTolerance are the design doc's §7 numbers: a
	// row counts as correct when its parsed points land within 1% of the
	// hand-checked value, and at least 95% of rows must.
	gateRowAccuracy = 0.95
	gateTolerance   = 0.01

	// gateMinRows keeps a thin transcription from passing vacuously. At 94
	// ranked rows (what recon measured) 95% allows four bad rows; below
	// twenty, one bad row already breaks the threshold and the percentage
	// stops meaning anything. This is the same guard as the M1 gate's
	// "corpus has at least 200 frames" check, for the same reason: a
	// threshold met by too small a sample proves nothing.
	gateMinRows = 20
)

// expectedCapture is the hand-checked ground truth, transcribed from the
// screenshots rather than from ingest's own output — checking a pipeline
// against itself proves nothing, and this project has already paid for that
// lesson once, when a screen labelled `vs` stayed self-consistently wrong for
// three weeks because nothing in the corpus could disagree with it.
//
// See fixtures/m4gate/README.md for how to produce one.
type expectedCapture struct {
	// Capture is the id of the capture on the machine this was transcribed
	// from. It is provenance only: the gate seeds its own capture row in
	// lw_manager_test and never looks this id up.
	Capture int64 `yaml:"capture"`

	PeriodKey string `yaml:"period_key"`

	// GameVersion is what later explains a gate that used to pass and now
	// does not — the same field, for the same reason, as the one
	// fixtures/corpus/index.yaml carries.
	GameVersion string `yaml:"game_version"`

	Alliance expectedAlliance `yaml:"alliance"`
	Frames   []expectedFrame  `yaml:"frames"`
	Rows     []expectedRow    `yaml:"rows"`
}

type expectedAlliance struct {
	Tag  string `yaml:"tag"`
	Name string `yaml:"name"`
}

// expectedFrame names one captured screenshot by content hash. There is no
// object_key field because there is nothing to record: keys are derived from
// the digest by blob.Key, so a frame reference cannot drift away from the
// bytes it was transcribed against.
//
// OffsetPx is stored as measured at capture time. Ingest converts it into row
// positions, so transcribing it wrong misaligns every row after it — copy it
// from capture_frames rather than re-deriving it.
type expectedFrame struct {
	Seq      int    `yaml:"seq"`
	SHA256   string `yaml:"sha256"`
	OffsetPx int    `yaml:"offset_px"`
}

// expectedRow is one row of the weekly ranking as a human read it. Rank is
// recorded for the transcriber's benefit — it makes a skipped or doubled row
// obvious on review — and is not asserted: ingest writes facts keyed by
// member, not by screen position.
type expectedRow struct {
	Rank   int     `yaml:"rank"`
	Name   string  `yaml:"name"`
	Points float64 `yaml:"points"`
}

func TestM4IngestGate(t *testing.T) {
	ctx := context.Background()

	exp := loadExpected(t)
	blobs := gateBlobs(t, ctx, exp)

	engine := ocr.NewTesseractEngine()
	if !engine.Available() {
		t.Skip("tesseract is not on PATH; install it with `apt install tesseract-ocr tesseract-ocr-eng` (Debian/Ubuntu) and re-run")
	}

	pool := gatePool(t, ctx)
	captureID, memberIDs := seedCapture(t, ctx, pool, exp)

	res, err := ingest.New(pool, blobs, engine).IngestVS(ctx, captureID, exp.PeriodKey)
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}

	// --- Condition 1: parsed values match the hand-checked ones.
	facts, err := pool.LiveFacts(ctx, "vs_points", exp.PeriodKey)
	if err != nil {
		t.Fatalf("LiveFacts: %v", err)
	}
	// Scoped to the members this run seeded. lw_manager_test is never
	// truncated between runs, so an earlier gate run's facts share this
	// period key; their member ids do not.
	got := map[int64]float64{}
	for _, f := range facts {
		got[f.MemberID] = f.Value
	}

	var wrong []string
	for _, row := range exp.Rows {
		id := memberIDs[row.Name]
		value, ok := got[id]
		switch {
		case !ok:
			wrong = append(wrong, fmt.Sprintf("rank %d %q: no fact written (want %.0f)", row.Rank, row.Name, row.Points))
		case !withinTolerance(value, row.Points):
			wrong = append(wrong, fmt.Sprintf("rank %d %q: read %.0f, want %.0f", row.Rank, row.Name, value, row.Points))
		}
	}

	accuracy := float64(len(exp.Rows)-len(wrong)) / float64(len(exp.Rows))
	if accuracy < gateRowAccuracy {
		sort.Strings(wrong)
		t.Errorf("M4 gate condition 1: %d/%d rows within %.0f%% (%.4f) is below %.2f\n\n%s",
			len(exp.Rows)-len(wrong), len(exp.Rows), gateTolerance*100, accuracy, gateRowAccuracy,
			join(wrong))
	}

	// --- Condition 2: nothing was dropped silently.
	//
	// This compares counts rather than pairing each discrepancy to its own
	// review row, because a review row records a screen position and its raw
	// text, not a member — an unmatched name has no member to key on, which
	// is the whole reason it is in the queue. The count comparison still
	// catches the failure the condition exists for: a row read wrongly but
	// confidently produces a discrepancy and no review row at all. It cannot
	// catch the inverse — reviews raised for unrelated reasons padding the
	// total — so read the two numbers together rather than the verdict alone.
	pending, err := pool.PendingReviews(ctx)
	if err != nil {
		t.Fatalf("PendingReviews: %v", err)
	}
	queued := 0
	for _, item := range pending {
		if item.CaptureID == captureID {
			queued++
		}
	}
	if queued < len(wrong) {
		sort.Strings(wrong)
		t.Errorf("M4 gate condition 2: %d rows disagree with the hand-checked capture but only %d reached the review queue; %d were dropped silently\n\n%s",
			len(wrong), queued, len(wrong)-queued, join(wrong))
	}

	// --- Condition 3: the capture still reconciles to complete.
	//
	// Worth being precise about what this proves. IngestVS never recomputes
	// completeness — captures.status was set at capture time by the route
	// that proved (or failed to prove) it reached the list bottom, and this
	// gate seeds it complete because the transcribed capture was. So this
	// does not re-derive completeness; it asserts that ingest did not
	// downgrade or error out a capture that arrived complete, and that it
	// counted rows while doing so.
	if res.Status != "complete" {
		t.Errorf("M4 gate condition 3: IngestVS reported status %q, want %q", res.Status, "complete")
	}
	after, err := pool.Capture(ctx, captureID)
	if err != nil {
		t.Fatalf("re-reading capture %d: %v", captureID, err)
	}
	if after.Status != "complete" {
		t.Errorf("M4 gate condition 3: capture %d persisted status %q, want %q", captureID, after.Status, "complete")
	}
	if after.ParsedRows == 0 {
		t.Errorf("M4 gate condition 3: capture %d finished with parsed_rows = 0", captureID)
	}

	t.Logf("M4 gate: %d/%d rows within %.0f%%, matched=%d queued=%d zeroed=%d status=%s (game version %s)",
		len(exp.Rows)-len(wrong), len(exp.Rows), gateTolerance*100,
		res.Matched, res.Queued, res.Zeroed, res.Status, exp.GameVersion)
}

// withinTolerance is the ±1% comparison. An expected zero is compared
// exactly: the weekly ranking lists only members who scored, so a
// hand-checked zero should not appear at all, and a relative tolerance around
// zero would accept any value.
func withinTolerance(got, want float64) bool {
	if want == 0 {
		return got == 0
	}
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff/want <= gateTolerance
}

// loadExpected reads the hand-checked ground truth, skipping when it has not
// been transcribed yet — the same shape as the M1 gate skipping an unpulled
// corpus, and for the same reason: the artifact is deliberately not in git.
func loadExpected(t *testing.T) expectedCapture {
	t.Helper()
	path := envOr("LW_M4GATE_EXPECTED", filepath.Join("..", "..", "fixtures", "m4gate", "expected.yaml"))
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no hand-checked capture at %s; transcribe one first (see fixtures/m4gate/README.md)", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var exp expectedCapture
	if err := yaml.Unmarshal(data, &exp); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	// Shape checks, before anything expensive runs. Each of these is a
	// transcription that would otherwise produce a confident, meaningless
	// verdict.
	switch {
	case exp.PeriodKey == "":
		t.Fatalf("%s: period_key is required", path)
	case exp.GameVersion == "":
		t.Fatalf("%s: game_version is required; it is what explains a gate that used to pass", path)
	case exp.Alliance.Tag == "" || exp.Alliance.Name == "":
		t.Fatalf("%s: alliance tag and name are required", path)
	case len(exp.Frames) == 0:
		t.Fatalf("%s: no frames listed", path)
	case len(exp.Rows) < gateMinRows:
		t.Fatalf("%s: %d rows transcribed, want at least %d; fewer cannot support a %.0f%% threshold",
			path, len(exp.Rows), gateMinRows, gateRowAccuracy*100)
	}

	seen := map[string]bool{}
	for _, row := range exp.Rows {
		if row.Name == "" {
			t.Fatalf("%s: rank %d has no name", path, row.Rank)
		}
		if seen[row.Name] {
			t.Fatalf("%s: %q is transcribed twice; the gate keys facts by member, so a duplicate name cannot be scored", path, row.Name)
		}
		seen[row.Name] = true
	}

	sort.Slice(exp.Frames, func(i, j int) bool { return exp.Frames[i].Seq < exp.Frames[j].Seq })
	return exp
}

// gateBlobs opens the configured blob store and confirms every transcribed
// frame is actually in it. Only cfg.Blob is used: the database side goes
// through internal/dbtest, which reads LW_TEST_DATABASE_URL and never the
// application's LW_DATABASE_URL, so a developer with that variable exported
// cannot point this gate at real data.
func gateBlobs(t *testing.T, ctx context.Context, exp expectedCapture) blob.Store {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load(): %v", err)
	}
	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		t.Fatalf("opening blob store (%s): %v", cfg.Blob.Backend, err)
	}
	for _, f := range exp.Frames {
		ok, err := blobs.Exists(ctx, blob.Key(f.SHA256))
		if err != nil {
			t.Fatalf("checking blob for frame %d: %v", f.Seq, err)
		}
		if !ok {
			t.Skipf("frame %d (%s) is not in the %s blob store; point LW_BLOB_* at the store the capture was written to",
				f.Seq, f.SHA256, cfg.Blob.Backend)
		}
	}
	return blobs
}

func gatePool(t *testing.T, ctx context.Context) *db.Pool {
	t.Helper()
	url, err := dbtest.Prepare(ctx, db.Migrate)
	if err != nil {
		t.Fatalf("dbtest.Prepare(): %v", err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedCapture builds the database state one real VS capture would have left
// behind: an alliance, its members, a screenshot row per frame pointing at
// the content-addressed blob, and a complete capture referencing them in
// scroll order. It returns the capture id and the member ids keyed by the
// hand-checked name.
//
// Members are seeded here rather than by running the roster route first,
// because IngestVS deliberately never creates one — a misread ranking row
// must not mint a member — so letting the roster route supply them would mean
// the gate silently measured two pipelines and blamed the wrong one on
// failure.
func seedCapture(t *testing.T, ctx context.Context, pool *db.Pool, exp expectedCapture) (int64, map[string]int64) {
	t.Helper()

	accountID := dbtest.SeedAccount(ctx, t, pool)

	// The alliance is scoped to this run rather than upserted on the
	// transcribed (tag, name). UpsertAlliance is idempotent on that pair, so a
	// shared alliance would accumulate every previous run's members in
	// lw_manager_test — which is never truncated between runs — and
	// IngestVS's zero-inference loop writes an inferred zero for every member
	// of the alliance it does not see. A second run would then report far
	// more zeroes than the capture has rows, which reads as a pipeline fault
	// rather than as test pollution. Measured before it was fixed: run two
	// zeroed 24 against a 13-row fixture.
	//
	// The transcribed tag and name stay in the fixture as provenance; nothing
	// here asserts on them.
	allianceID, err := pool.UpsertAlliance(ctx, db.Alliance{
		Tag:         fmt.Sprintf("%s-%d", exp.Alliance.Tag, accountID),
		Name:        fmt.Sprintf("%s (gate run %d)", exp.Alliance.Name, accountID),
		MemberCount: len(exp.Rows),
	})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}

	memberIDs := make(map[string]int64, len(exp.Rows))
	for _, row := range exp.Rows {
		id, err := pool.CreateMember(ctx, db.Member{
			AllianceID:     allianceID,
			Name:           row.Name,
			NameNormalized: roster.Normalize(row.Name),
		})
		if err != nil {
			t.Fatalf("CreateMember %q: %v", row.Name, err)
		}
		memberIDs[row.Name] = id
	}

	frames := make([]db.CaptureFrameInput, len(exp.Frames))
	for i, f := range exp.Frames {
		var shotID int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO screenshots (account_id, captured_at, object_key, sha256)
			 VALUES ($1, now(), $2, $3) RETURNING id`,
			accountID, blob.Key(f.SHA256), f.SHA256).Scan(&shotID); err != nil {
			t.Fatalf("seeding screenshot for frame %d: %v", f.Seq, err)
		}
		frames[i] = db.CaptureFrameInput{ScreenshotID: shotID, Seq: f.Seq, OffsetPx: f.OffsetPx}
	}

	// complete=true because the transcribed capture was: the route proved it
	// reached the bottom of the list. This is what lets IngestVS infer a zero
	// for a member who does not appear, and it is seeded rather than derived
	// — see condition 3's comment on what that does and does not prove.
	if err := pool.RecordCapture(ctx, accountID, "vs_ranking", frames, true); err != nil {
		t.Fatalf("RecordCapture: %v", err)
	}

	var captureID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM captures WHERE account_id = $1 AND route = 'vs_ranking'`, accountID,
	).Scan(&captureID); err != nil {
		t.Fatalf("reading back the seeded capture: %v", err)
	}
	return captureID, memberIDs
}

func join(lines []string) string {
	out := ""
	for _, l := range lines {
		out += "  " + l + "\n"
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
