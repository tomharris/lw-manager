//go:build m4rostergate

// The M4 roster gate: ingest reproduces a hand-transcribed roster capture.
//
// Behind a build tag for the same three reasons gate_test.go is — Postgres via
// internal/dbtest, the blob store holding the capture's frames, and the
// tesseract binary. Run it with `make gate-roster`.
//
// It asserts the four conditions from the design doc's §4. Two of them differ
// from gate-m4's, and the difference is not stylistic: the VS route MATCHES
// into a closed set, and this route CREATES. So the VS gate's "no
// misattribution" becomes "no splits" here, and its "the capture reconciles to
// complete" inverts into "reconciliation reports the shortfall truthfully" --
// a gate demanding `complete` cannot go green while the route is still
// climbing, and a gate that cannot go green is not a ratchet.
//
//  1. >= 95% of transcribed members exist in `members`, in the right group;
//  2. zero splits -- no two member rows claim the same transcribed member,
//     and no member row corresponds to nobody;
//  3. every transcribed member not created produced a review_queue row;
//  4. PerGroup tallies and Status describe what was parsed, truthfully.
package ingest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/dbtest"
	"github.com/tomharris/lw-manager/internal/ingest"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
	"github.com/tomharris/lw-manager/internal/vision"
)

// expectedRoster is the hand-transcribed ground truth. See
// fixtures/m4rostergate/README.md for how it is produced and why it is
// transcribed rather than exported.
type expectedRoster struct {
	// Capture is provenance only: the gate seeds its own capture row in
	// lw_manager_test and never looks this id up.
	Capture     int64                  `yaml:"capture"`
	PeriodKey   string                 `yaml:"period_key"`
	GameVersion string                 `yaml:"game_version"`
	Alliance    expectedRosterAlliance `yaml:"alliance"`
	Frames      []expectedRosterFrame  `yaml:"frames"`
	Groups      []expectedGroup        `yaml:"groups"`
	Members     []expectedMember       `yaml:"members"`
}

type expectedRosterAlliance struct {
	Tag  string `yaml:"tag"`
	Name string `yaml:"name"`
	// MemberCount is the "97" of "Members: 97/100" read off the alliance
	// frame. It is the alliance-total reconciliation's ground truth.
	MemberCount int `yaml:"member_count"`
	// Leader occupies the MEMBER LIST screen's banner and has no rank-group
	// row, while every other member has one. That is why the group totals sum
	// to one less than MemberCount, and recording the name is what makes the
	// loader's off-by-one check a statement about this screen's structure
	// rather than a fudge factor.
	Leader string `yaml:"leader"`
}

// expectedRosterFrame names one captured screenshot by content hash. OffsetPx
// is copied from capture_frames, never re-derived: it was measured against the
// frames as captured and ingest turns it into row positions, so a wrong value
// misaligns every row after it. GroupKey is vision.AllianceSummaryGroupKey on
// the one frame carrying the "Members: 97/100" line and empty on every
// member-list frame -- roster_capture deliberately asserts no group.
type expectedRosterFrame struct {
	Seq      int    `yaml:"seq"`
	SHA256   string `yaml:"sha256"`
	OffsetPx int    `yaml:"offset_px"`
	GroupKey string `yaml:"group_key"`
}

// expectedGroup is one rank group's own sticky header as a human read it.
// Total is the M of "10/64" -- the group's size, not its online count. It is
// ground truth here rather than something the gate infers, because the header
// count is the structural gate on member creation and is currently the
// route's dominant defect.
// Expanded records whether roster_capture opened this group during the run.
// Capture 1 has TWO collapsed groups, not one: R4 "This Is It" reads "2/9" and
// R1 "Danger Zone" reads "0/12", both carry the UP chevron, and neither is
// followed by a single row anywhere in the capture. Their twenty-one members
// are a group the capture saw and never opened, not twenty-one members the
// pipeline lost. The distinction is the whole reason this field exists --
// without it a collapsed group is indistinguishable from a catastrophic read
// failure.
type expectedGroup struct {
	Rank     string `yaml:"rank"`
	Name     string `yaml:"name"`
	Total    int    `yaml:"total"`
	Expanded bool   `yaml:"expanded"`
}

// expectedMember is one member row as a human read it, at full resolution,
// across every frame the member appears in.
//
// Power and Level are transcribed though the gate does not assert them: both
// fields yield zero facts today and are deferred to M6, and transcription is
// the expensive part of this fixture -- doing it twice would be indefensible.
// The probes read them.
//
// LastActive is the string the row shows: "Online", or an elapsed time like
// "3h ago". Note carries a transcriber's remark for a glyph that is a best
// reading rather than a certain one, on the model of the VS fixture's
// thirteen decorated names. A member carrying a Note is still asserted; the
// note is what a failure should be re-read against before the pipeline is
// blamed.
type expectedMember struct {
	Rank       string  `yaml:"rank"`
	Name       string  `yaml:"name"`
	Power      float64 `yaml:"power"`
	Level      int     `yaml:"level"`
	LastActive string  `yaml:"last_active"`
	Note       string  `yaml:"note,omitempty"`
}

// gateRosterMinMembers keeps a thin transcription from passing vacuously, the
// same guard gate_test.go's gateMinRows is and for the same reason: below
// twenty, one bad row already breaks a 95% threshold and the percentage stops
// meaning anything.
const gateRosterMinMembers = 20

// gateRosterCoverage is taken from gate-m4's bar rather than derived from what
// this route currently does. That ordering is the point: the VS gate's 95%
// came from the design doc and the pipeline had to climb 63/86 to 85/86 to
// reach it. A bar set after seeing the number is a bar fitted to the pipeline
// it was supposed to judge.
//
// It applies to TRANSCRIBED members (75 on capture 1), not to the alliance's
// own count (97). The two differ by the two groups this capture never opened
// -- R4 "This Is It" at 9 and R1 "Danger Zone" at 12 -- and by the leader's
// banner row, none of which is anything the pipeline could read from these
// pixels. Scoring against 97 would be scoring against frames the capture does
// not contain, and would make the bar unreachable by construction at anything
// above 77.3%.
//
// The cost is worth stating rather than burying: R4, R1 and the leader are
// never exercised here, so a defect specific to a collapsed group or to the
// banner goes uncaught until a capture that expands them exists.
const gateRosterCoverage = 0.95

func TestRosterGateFixtureShape(t *testing.T) {
	exp := loadExpectedRoster(t, filepath.Join("testdata", "rostergate_shape.yaml"))
	if len(exp.Members) != 3 {
		t.Fatalf("shape fixture has %d members, want 3", len(exp.Members))
	}
	if exp.Groups[0].Total != 2 {
		t.Fatalf("group %q total = %d, want 2", exp.Groups[0].Rank, exp.Groups[0].Total)
	}
}

// TestRosterGateGroundTruthShape runs the real transcription through the same
// validator. The shape fixture above proves parseExpectedRoster works; this
// proves the artifact the gate will actually read is well formed, which is a
// separate claim and the one that matters -- a fixture that fails to load
// turns the gate into a skip, and a skip is indistinguishable from a pass in
// a CI summary.
//
// It skips when the file is absent so a fresh clone still builds, the same
// shape as the M1 gate skipping an unpulled corpus.
func TestRosterGateGroundTruthShape(t *testing.T) {
	exp := loadExpectedRoster(t, filepath.Join("..", "..", "fixtures", "m4rostergate", "expected.yaml"))
	if len(exp.Members) < gateRosterMinMembers {
		t.Fatalf("%d members transcribed, want at least %d; fewer cannot support a 95%% threshold",
			len(exp.Members), gateRosterMinMembers)
	}
	t.Logf("ground truth: %d members across %d groups, alliance count %d, game version %s",
		len(exp.Members), len(exp.Groups), exp.Alliance.MemberCount, exp.GameVersion)
}

// loadExpectedRoster reads and validates the ground truth, skipping when it
// has not been transcribed yet -- the same shape as the M1 gate skipping an
// unpulled corpus, and for the same reason: the artifact is deliberately not
// generated. It is a thin t.Fatalf/t.Skipf wrapper around parseExpectedRoster,
// which does the actual validation and is what TestParseExpectedRosterRejects
// below exercises directly -- a validator that returns errors is testable,
// and one that calls t.Fatalf is not.
func loadExpectedRoster(t *testing.T, path string) expectedRoster {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no hand-transcribed roster at %s; transcribe one first (see fixtures/m4rostergate/README.md)", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	exp, err := parseExpectedRoster(data, path)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return exp
}

// parseExpectedRoster validates the ground truth's shape and returns the
// first violation it finds. Every check here is a transcription mistake that
// would otherwise produce a confident, meaningless verdict from the gate --
// see fixtures/m4rostergate/README.md for the transcription process each of
// these guards against getting wrong.
func parseExpectedRoster(data []byte, path string) (expectedRoster, error) {
	var exp expectedRoster
	if err := yaml.Unmarshal(data, &exp); err != nil {
		return expectedRoster{}, fmt.Errorf("parsing %s: %w", path, err)
	}

	switch {
	case exp.PeriodKey == "":
		return expectedRoster{}, fmt.Errorf("%s: period_key is required", path)
	case exp.GameVersion == "":
		return expectedRoster{}, fmt.Errorf("%s: game_version is required; it is what explains a gate that used to pass", path)
	case exp.Alliance.Tag == "" || exp.Alliance.Name == "":
		return expectedRoster{}, fmt.Errorf("%s: alliance tag and name are required", path)
	case exp.Alliance.MemberCount <= 0:
		return expectedRoster{}, fmt.Errorf("%s: alliance member_count is required; it is the alliance-total reconciliation's ground truth", path)
	case len(exp.Frames) == 0:
		return expectedRoster{}, fmt.Errorf("%s: no frames listed", path)
	case len(exp.Groups) == 0:
		return expectedRoster{}, fmt.Errorf("%s: no groups listed; group headers are ground truth here, not inferred", path)
	}

	// Frame checks: no duplicated seq or sha256, and GroupKey is either empty
	// or the alliance-summary sentinel, carried by exactly one frame.
	//
	// The dup checks are not a regression fix -- gate_test.go has the same
	// gap -- but this fixture's frame list is roughly three times the VS
	// fixture's (~62 against 21), and Task 3 builds it by copying rows from
	// the capture_frames query in fixtures/m4rostergate/README.md, which is
	// exactly where a duplicated or skipped seq comes from.
	//
	// The GroupKey check exists because loadExpectedRoster never looked at
	// the field until now despite its own doc comment calling it load-bearing
	// -- a typo like "_aliance_summary" is precisely the "confident,
	// meaningless verdict" every other check here exists to prevent.
	seenSeq := map[int]bool{}
	seenSHA256 := map[string]bool{}
	summaryFrames := 0
	for _, f := range exp.Frames {
		if seenSeq[f.Seq] {
			return expectedRoster{}, fmt.Errorf("%s: frame seq %d appears twice", path, f.Seq)
		}
		seenSeq[f.Seq] = true
		if seenSHA256[f.SHA256] {
			return expectedRoster{}, fmt.Errorf("%s: frame sha256 %q appears twice (seq %d); the same frame cannot be listed under two positions", path, f.SHA256, f.Seq)
		}
		seenSHA256[f.SHA256] = true

		if f.GroupKey != "" && f.GroupKey != vision.AllianceSummaryGroupKey {
			return expectedRoster{}, fmt.Errorf("%s: frame %d has group_key %q, want either empty or %q", path, f.Seq, f.GroupKey, vision.AllianceSummaryGroupKey)
		}
		if f.GroupKey == vision.AllianceSummaryGroupKey {
			summaryFrames++
		}
	}
	if summaryFrames != 1 {
		return expectedRoster{}, fmt.Errorf("%s: %d frames carry group_key %q, want exactly 1 -- it marks the single alliance-summary frame roster_capture writes once per run", path, summaryFrames, vision.AllianceSummaryGroupKey)
	}

	// The group totals plus the leader must equal the alliance count. This is
	// the transcription's own internal check and it is worth more than it
	// looks: it is the one arithmetic relation the screen states twice, so a
	// disagreement means a header was misread or a group was missed entirely,
	// and either would silently weaken every condition the gate then asserts.
	//
	// Plus the leader, not equal outright: the leader occupies the MEMBER LIST
	// banner and has no rank-group row, while every other member has one. On
	// capture 1 that is 9 + 64 + 11 + 12 = 96 against a "Members: 97/100"
	// line. An alliance where that relation does not hold fails here loudly,
	// which is the right failure -- it means the screen's structure changed
	// and every group-keyed assumption in this package needs re-reading.
	sum := 0
	seenRank := map[string]bool{}
	for _, g := range exp.Groups {
		if g.Rank == "" {
			return expectedRoster{}, fmt.Errorf("%s: a group has no rank", path)
		}
		if seenRank[g.Rank] {
			return expectedRoster{}, fmt.Errorf("%s: rank %q appears twice; groups are keyed by rank", path, g.Rank)
		}
		seenRank[g.Rank] = true
		// Strictly positive, collapsed or not: a rank group's sticky header
		// states its size whether the group is open or closed. R1 Danger
		// Zone's real header reads "0/12" -- the 0 is how many members are
		// currently online, the 12 is the group's size, and a group with zero
		// members would not render a header to read at all. So total is
		// always positive for a group that exists, and relaxing this to allow
		// zero would give up a check that catches exactly the misread this
		// milestone's dominant defect lives in: a garbled M in "N/M".
		if g.Total <= 0 {
			return expectedRoster{}, fmt.Errorf("%s: group %q has total %d, want a positive size", path, g.Rank, g.Total)
		}
		sum += g.Total
	}
	if exp.Alliance.Leader == "" {
		return expectedRoster{}, fmt.Errorf("%s: alliance leader is required; the sum check below is a statement about the leader having no group row", path)
	}
	if sum+1 != exp.Alliance.MemberCount {
		return expectedRoster{}, fmt.Errorf("%s: group totals sum to %d, +1 for leader %q = %d, but the alliance frame reads %d; a header was misread, a group is missing, or this screen no longer puts exactly one member in the banner",
			path, sum, exp.Alliance.Leader, sum+1, exp.Alliance.MemberCount)
	}

	// Every transcribed member must belong to a group the capture EXPANDED.
	// A member listed under a collapsed group is a transcription error by
	// construction: the capture contains no row for them to have been read
	// from, so asserting them would fail the gate on frames that do not exist.
	expanded := map[string]bool{}
	for _, g := range exp.Groups {
		expanded[g.Rank] = g.Expanded
	}

	seenName := map[string]bool{}
	countByRank := map[string]int{}
	for _, m := range exp.Members {
		if m.Name == "" {
			return expectedRoster{}, fmt.Errorf("%s: a member in group %q has no name", path, m.Rank)
		}
		if !seenRank[m.Rank] {
			return expectedRoster{}, fmt.Errorf("%s: member %q is in group %q, which is not in the groups list", path, m.Name, m.Rank)
		}
		if !expanded[m.Rank] {
			return expectedRoster{}, fmt.Errorf("%s: member %q is in group %q, which this capture never expanded; there is no row to have read them from", path, m.Name, m.Rank)
		}
		if seenName[m.Name] {
			return expectedRoster{}, fmt.Errorf("%s: %q is transcribed twice; the gate keys members by name, so a duplicate cannot be scored", path, m.Name)
		}
		seenName[m.Name] = true
		countByRank[m.Rank]++
	}

	// Per-group member count, strict, for every group the capture expanded.
	// Nothing above catches a transcription that drops one member from R4 and
	// adds a spurious one under R3 -- that nets to zero at the alliance-sum
	// check above and passes silently otherwise.
	//
	// Strict in both directions, deliberately, and for different reasons.
	// count > Total is impossible outright: a group cannot contain more
	// members than its own header states. count < Total is possible in
	// principle -- a scroll that never reached the group's end -- but at
	// transcription time it overwhelmingly means members were missed, and
	// this project would rather that be forced into the open during Task 3's
	// transcription than smoothed over by a lenient guard.
	for _, g := range exp.Groups {
		if !g.Expanded {
			continue
		}
		count := countByRank[g.Rank]
		switch {
		case count > g.Total:
			return expectedRoster{}, fmt.Errorf("%s: group %q (%s) has %d transcribed members, but its header states a total of %d; a group cannot contain more members than its own header states",
				path, g.Rank, g.Name, count, g.Total)
		case count < g.Total:
			return expectedRoster{}, fmt.Errorf("%s: group %q (%s) has %d transcribed members against a header total of %d; a scroll that never reached the group's end is possible in principle, but at transcription time this overwhelmingly means members were missed",
				path, g.Rank, g.Name, count, g.Total)
		}
	}

	sort.Slice(exp.Frames, func(i, j int) bool { return exp.Frames[i].Seq < exp.Frames[j].Seq })
	return exp, nil
}

// TestParseExpectedRosterRejects is the mutation checks from this file's
// earlier development, made permanent. Each case in the earlier round was
// hand-applied to the shape fixture, observed to fail, then reverted -- which
// proved the guard could fail once, not that it stays able to. This is the
// same lesson CLAUDE.md already records for rankBadgeMinGap: a guard that is
// only ever mutation-checked by hand is not regression-tested, and nothing
// commits to catch it being accidentally weakened later.
//
// It covers every guard above that encodes a domain claim about the roster
// screen -- not the trivial "field is empty" checks, which fail the same way
// regardless of what the field means.
func TestParseExpectedRosterRejects(t *testing.T) {
	// base is a minimal valid fixture. sum(2, 1) + 1 leader = 4 = member_count.
	// R4 is expanded with exactly 2 transcribed members (matching its total);
	// R3 is collapsed with none. Every case below mutates exactly one thing
	// out of this valid shape, so a failure can only be attributed to the
	// check the case names.
	const base = `
period_key: "2026-W33"
game_version: "0.0.0"
alliance:
  tag: "TEST"
  name: "Shape Fixture"
  member_count: 4
  leader: "TheLeader"
frames:
  - seq: 0
    sha256: "aaa"
    offset_px: 0
    group_key: "_alliance_summary"
  - seq: 1
    sha256: "bbb"
    offset_px: 100
    group_key: ""
groups:
  - rank: "R4"
    name: "Alpha"
    total: 2
    expanded: true
  - rank: "R3"
    name: "Beta"
    total: 1
    expanded: false
members:
  - rank: "R4"
    name: "MemberOne"
  - rank: "R4"
    name: "MemberTwo"
`

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "group totals plus leader disagree with member_count",
			yaml:    strings.Replace(base, "member_count: 4", "member_count: 5", 1),
			wantErr: "group totals sum to",
		},
		{
			name: "member listed under a group that was never expanded",
			yaml: strings.Replace(base,
				"  - rank: \"R4\"\n    name: \"MemberTwo\"\n",
				"  - rank: \"R4\"\n    name: \"MemberTwo\"\n  - rank: \"R3\"\n    name: \"MemberThree\"\n",
				1),
			wantErr: "which this capture never expanded",
		},
		{
			name:    "a group has total 0",
			yaml:    strings.Replace(base, "total: 1\n    expanded: false", "total: 0\n    expanded: false", 1),
			wantErr: "want a positive size",
		},
		{
			name: "expanded group's member count is below its total",
			yaml: strings.Replace(base,
				"  - rank: \"R4\"\n    name: \"MemberOne\"\n  - rank: \"R4\"\n    name: \"MemberTwo\"\n",
				"  - rank: \"R4\"\n    name: \"MemberOne\"\n",
				1),
			wantErr: "overwhelmingly means members were missed",
		},
		{
			name: "expanded group's member count is above its total",
			yaml: strings.Replace(base,
				"  - rank: \"R4\"\n    name: \"MemberTwo\"\n",
				"  - rank: \"R4\"\n    name: \"MemberTwo\"\n  - rank: \"R4\"\n    name: \"MemberThree\"\n",
				1),
			wantErr: "cannot contain more members than its own header states",
		},
		{
			name:    "a frame's group_key is neither empty nor the sentinel",
			yaml:    strings.Replace(base, `group_key: ""`, `group_key: "_aliance_summary"`, 1),
			wantErr: "want either empty or",
		},
		{
			name:    "no frame carries the alliance-summary sentinel",
			yaml:    strings.Replace(base, `group_key: "_alliance_summary"`, `group_key: ""`, 1),
			wantErr: "want exactly 1",
		},
		{
			name:    "two frames carry the alliance-summary sentinel",
			yaml:    strings.Replace(base, `group_key: ""`, `group_key: "_alliance_summary"`, 1),
			wantErr: "want exactly 1",
		},
		{
			name:    "two frames share the same seq",
			yaml:    strings.Replace(base, "seq: 1", "seq: 0", 1),
			wantErr: "appears twice",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseExpectedRoster([]byte(tc.yaml), "malformed.yaml")
			if err == nil {
				t.Fatalf("parseExpectedRoster: got no error, want one containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parseExpectedRoster error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestM4RosterGate(t *testing.T) {
	ctx := context.Background()

	exp := loadExpectedRoster(t, filepath.Join("..", "..", "fixtures", "m4rostergate", "expected.yaml"))
	if len(exp.Members) < gateRosterMinMembers {
		t.Fatalf("%d members transcribed, want at least %d", len(exp.Members), gateRosterMinMembers)
	}
	blobs := rosterGateBlobs(t, ctx, exp)

	engine := ocr.NewTesseractEngine()
	if !engine.Available() {
		t.Skip("tesseract is not on PATH; install it with `apt install tesseract-ocr tesseract-ocr-eng` (Debian/Ubuntu) and re-run")
	}

	pool := gatePool(t, ctx)
	captureID := seedRosterCapture(t, ctx, pool, exp)

	res, err := ingest.New(pool, blobs, engine).IngestRoster(ctx, captureID, exp.PeriodKey)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	allianceID, err := pool.CurrentAllianceID(ctx)
	if err != nil {
		t.Fatalf("CurrentAllianceID: %v", err)
	}
	created, err := pool.ListMembers(ctx, allianceID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}

	// Correspondence between a created member row and a transcribed member is
	// judged by roster.Rank at AutoAccept -- the same scorer the pipeline
	// uses, deliberately. A cosmetically wrong display name is recoverable:
	// ALBANSO for a real ALBAN80 is a documented confusable, so a later VS
	// ingest reading the name correctly matches it to that same row and the
	// facts land in one place. Scoring by exact string would fail the gate on
	// a defect that costs nothing, and hide the one that costs everything.
	//
	// roster.Member carries no NameNormalized field: TokenSetRatio normalizes
	// internally, so the truth set is built from display names alone. IDs are
	// synthetic indices -- nothing here keys on them, and Rank requires the
	// field to exist.
	// Before any of that runs, the truth set has to be separable by the scorer
	// that is about to score against it. If two transcribed names reach
	// AutoAccept against each other, two correct member rows both resolve to
	// the same truth entry: condition 2 reports a split -- a hard zero -- and
	// condition 1 reports the other member as never created. Both failures
	// would be the gate blaming the pipeline for a defect in the fixture, and
	// nothing in the output would say so.
	//
	// t.Fatalf, not t.Errorf, and before the scoring loop rather than beside
	// it: every number this gate then prints is computed through the
	// correspondence that just failed to be well defined, so continuing would
	// produce a full, confident, meaningless report. make probe-m4 already
	// fails on the same measure over the VS roster, for the same reason
	// (roster.ClosestPairScore's own doc comment makes the argument).
	names := make([]string, 0, len(exp.Members))
	for _, m := range exp.Members {
		names = append(names, m.Name)
	}
	closestPair, pairA, pairB := roster.ClosestPairScore(names)
	if closestPair >= roster.AutoAccept {
		t.Fatalf("this fixture cannot be scored: transcribed members %q and %q score %d against each other, at or above AutoAccept %d, so a correct read of either resolves to both -- give one an alias or re-read the transcription before believing any number from this gate",
			pairA, pairB, closestPair, roster.AutoAccept)
	}
	// Logged whether or not it fires, on probe-m4's model: the margin is the
	// budget every later change to confusableCost or the pair table spends,
	// and a headline that only appears on failure gives nobody a baseline to
	// read the next one against.
	t.Logf("roster gate truth set: %d members, closest pair %q/%q at %d against AutoAccept %d",
		len(exp.Members), pairA, pairB, closestPair, roster.AutoAccept)

	truth := make([]roster.Member, 0, len(exp.Members))
	for i, m := range exp.Members {
		truth = append(truth, roster.Member{ID: int64(i + 1), Name: m.Name})
	}

	claimedBy := map[string][]string{} // transcribed name -> member rows claiming it
	var orphans []string
	for _, m := range created {
		cands := roster.Rank(m.Name, truth) // sorted best-first
		switch {
		case len(cands) == 0:
			t.Fatalf("roster.Rank returned nothing for %q against %d transcribed members", m.Name, len(truth))
		case cands[0].Score < roster.AutoAccept:
			orphans = append(orphans, fmt.Sprintf("%q (best %q at %d, below AutoAccept %d)",
				m.Name, cands[0].Name, cands[0].Score, roster.AutoAccept))
		default:
			claimedBy[cands[0].Name] = append(claimedBy[cands[0].Name], m.Name)
		}
	}

	// --- Condition 1: coverage.
	//
	// A miss names the group the member was actually created in alongside the
	// one they belong to, not just the fact of the miss. The two failure
	// shapes need opposite fixes -- never created is a read or match failure,
	// created in the wrong group is an attribution failure -- and a count
	// alone cannot tell them apart. Sticky-header lag is the known instance:
	// IngestRoster takes a frame's rank from that frame's own sticky header,
	// which still reads the outgoing group while the rows beneath it already
	// belong to the next one, so the next group's opening rows land under the
	// previous group's rank. That pattern is only visible if the assigned
	// group is printed.
	rankOf := map[string]string{}
	for _, m := range created {
		rankOf[m.Name] = m.Rank
	}

	// The two shapes are collected separately as well as together. Condition 1
	// wants both -- a member in the wrong group is not covered -- but condition
	// 3 wants only neverCreated, for the reason argued at its own site.
	var neverCreated, wrongGroup []string
	covered := 0
	for _, m := range exp.Members {
		rows := claimedBy[m.Name]
		if len(rows) == 0 {
			neverCreated = append(neverCreated, fmt.Sprintf("%s %q: never created", m.Rank, m.Name))
			continue
		}
		if got := rankOf[rows[0]]; got != m.Rank {
			wrongGroup = append(wrongGroup, fmt.Sprintf("%s %q: created in group %q", m.Rank, m.Name, got))
			continue
		}
		covered++
	}
	missing := append(append([]string{}, neverCreated...), wrongGroup...)
	coverage := float64(covered) / float64(len(exp.Members))
	if coverage < gateRosterCoverage {
		sort.Strings(missing)
		t.Errorf("roster gate condition 1: %d/%d members covered (%.4f) is below %.2f\n\n%s",
			covered, len(exp.Members), coverage, gateRosterCoverage, join(missing))
	}

	// --- Condition 2: zero splits.
	//
	// Two failures, one hard zero each. An orphan is a member row matching
	// nobody on the roster -- a person invented from a misread. A split is the
	// same person minted twice under two different reads, and it is the worse
	// of the two: their facts divide across two rows and no review-queue
	// resolution rejoins them. This is the roster route's equivalent of a VS
	// misattribution, and like it, it gets a hard zero rather than a
	// percentage.
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("roster gate condition 2: %d member rows correspond to nobody transcribed\n\n%s", len(orphans), join(orphans))
	}
	var splits []string
	for name, rows := range claimedBy {
		if len(rows) > 1 {
			sort.Strings(rows)
			splits = append(splits, fmt.Sprintf("%q claimed by %d rows: %v", name, len(rows), rows))
		}
	}
	if len(splits) > 0 {
		sort.Strings(splits)
		t.Errorf("roster gate condition 2: %d transcribed members are split across two member rows\n\n%s", len(splits), join(splits))
	}

	// --- Condition 3: nothing dropped silently.
	//
	// Both sides of this comparison are narrowed to the name class, and each
	// narrowing fixes a different way the raw counts lie.
	//
	// The right side counts only name-class review reasons. writeFacts queues
	// up to three FIELD-level rows per successfully matched row -- an
	// unparseable power, a level below the confidence gate -- so the
	// capture-wide review count is dominated by traffic that has no
	// relationship to a member going missing. Measured on capture 1's first
	// run: 224 review rows against roughly 120 parsed rows. A comparison
	// against that number is satisfied by field noise and asserts nothing.
	//
	// The left side counts only members that were never created. A member
	// created under the wrong group is not dropped: the row was read, matched
	// and its facts written, and by construction it queued no name review at
	// all. Counting it as "dropped" would make the left side unanswerable by
	// any correct pipeline behaviour. Wrong-group attribution is condition 1's
	// failure, and condition 1 names it.
	//
	// What is left is a real, if weak, claim: a member the pipeline failed to
	// produce should have left a name-class row behind saying so. It is still
	// a count comparison rather than a pairing -- a review row records a
	// screen position and its raw text, not a member, because an unmatched
	// name has no member to key on -- so it cannot prove the queued rows are
	// the missing members. It catches the failure it exists for, a member lost
	// with nothing in the queue at all, and no more than that.
	pending, err := pool.PendingReviews(ctx)
	if err != nil {
		t.Fatalf("PendingReviews: %v", err)
	}
	nameReviewReasons := map[string]bool{
		"unreadable_name":               true,
		"ambiguous_name_match":          true,
		"low_confidence_name":           true,
		"no_confident_match_group_full": true,
	}
	queued, nameQueued := 0, 0
	for _, item := range pending {
		if item.CaptureID != captureID {
			continue
		}
		queued++
		if nameReviewReasons[item.Reason] {
			nameQueued++
		}
	}
	if nameQueued < len(neverCreated) {
		t.Errorf("roster gate condition 3: %d members were never created but only %d name-class rows reached the review queue (%d rows queued in total, all reasons); %d were dropped silently",
			len(neverCreated), nameQueued, queued, len(neverCreated)-nameQueued)
	}

	// --- Condition 4: reconciliation reports truthfully.
	//
	// NOT "the capture is complete". Reconciliation marks any group-count
	// mismatch partial, so a gate demanding complete cannot go green while the
	// route is still climbing, and a gate that cannot go green is not a
	// ratchet. Reconciliation is a reporting mechanism; what is worth
	// asserting is that its report is honest.
	//
	// Three things are asserted, and they are all about the REPORT: that every
	// group the capture contains is described at all, that the size it is
	// described with is the size its own header states, and that Status
	// follows from those tallies by the rule IngestRoster actually applies.
	//
	// A per-group CREATED-count check was tried here and removed. GroupTally's
	// MatchedOrCreated counts row EVENTS -- every band that resolved to a
	// member, including the same member re-matched on a later frame's overlap
	// -- and this fixture knows only members. On capture 1 that check read
	// "R3 reported created=83, but 45 of its transcribed members are in
	// members" and called it a lie; the 38-row gap was 26 legitimate
	// re-matches, 7 orphans and 5 rows correctly counted under the group the
	// pipeline had assigned them to. Reconciliation had reported exactly what
	// happened. Putting two aggregates side by side and calling the difference
	// a defect is the error CLAUDE.md names outright, and the gate committed
	// it. There is no rescoping that saves it either: with no independent row
	// count in the fixture, such a check can only agree once conditions 1 and
	// 2 are already green, and adds no signal before then. The attribution
	// question it was reaching for is condition 1's "created in group X".
	//
	// Capture 1 has TWO collapsed groups, R4 "This Is It" (9) and R1 "Danger
	// Zone" (12), both transcribed with expanded: false and no members. Their
	// headers are legible, so a healthy pipeline gives each a tally reading
	// Expected N / Parsed 0, which is a shortfall and makes the capture
	// correctly `partial`. That is not a failure to excuse -- it is precisely
	// what this condition exists to assert, and a capture that reported
	// `complete` with two groups it never opened would be the defect.
	var lies []string
	for _, g := range exp.Groups {
		tally, seen := res.PerGroup[g.Rank]
		if !seen {
			// Unconditional. This was once gated on the review queue being
			// empty, which silenced it on capture 1 -- three of four groups
			// produced no tally and the condition designed to catch a
			// reconciliation misdescription said nothing, because `queued` is
			// capture-wide and 224 field-level rows kept it non-zero. The
			// guard was not even scoped to this group, so the message's claim
			// about review rows would have been unfounded whenever it did
			// fire. A run that describes a four-group capture with one group's
			// tally IS a misdescription, whatever else reached the queue.
			lies = append(lies, fmt.Sprintf("group %s (%q, %d members) produced no tally at all; reconciliation describes this capture without it", g.Rank, g.Name, g.Total))
			continue
		}
		if tally.Expected != g.Total {
			lies = append(lies, fmt.Sprintf("group %s: reported expected=%d, transcribed total is %d", g.Rank, tally.Expected, g.Total))
		}
	}

	// Status, re-derived from the tallies by IngestRoster's own rule, which
	// has TWO halves (roster.go, after the frame loop): a shortfall in any
	// group that was seen, OR an alliance-total check that was performed and
	// disagreed. Modelling only the first half would be inert on this capture
	// -- two collapsed groups force a shortfall regardless -- and wrong on a
	// fully-expanded one, where the gate would demand `complete` while
	// production correctly said `partial`, and condition 4 would report a lie
	// that is not one.
	//
	// What this asserts is narrower than it looks, and worth stating: both
	// sides are computed from the same run's output, so it cannot catch a
	// tally that is itself wrong -- that is what the Expected-vs-header check
	// above is for. It catches a Status that contradicts the numbers printed
	// beside it.
	//
	// totalParsed inside IngestRoster is incremented once per collected band,
	// the same event that increments the band's group tally, so summing Parsed
	// over PerGroup reproduces it exactly.
	groupShortfall, sumParsed := false, 0
	for _, tally := range res.PerGroup {
		sumParsed += tally.Parsed
		if tally.Parsed != tally.Expected {
			groupShortfall = true
		}
	}
	allianceShortfall := res.AllianceTotalChecked && sumParsed != res.AllianceMemberCount
	wantStatus := "complete"
	if groupShortfall || allianceShortfall {
		wantStatus = "partial"
	}
	if res.Status != wantStatus {
		lies = append(lies, fmt.Sprintf("status is %q; group shortfall = %v, alliance-total shortfall = %v (checked=%v, parsed %d against %d), so it should be %q",
			res.Status, groupShortfall, allianceShortfall, res.AllianceTotalChecked, sumParsed, res.AllianceMemberCount, wantStatus))
	}
	if len(lies) > 0 {
		sort.Strings(lies)
		t.Errorf("roster gate condition 4: reconciliation does not describe what was parsed\n\n%s", join(lies))
	}

	// never_created and wrong_group are printed apart because they are the two
	// halves of condition 1 that need opposite fixes, and name_queued beside
	// queued is what makes condition 3's verdict readable: queued is every
	// reason, name_queued is the only part of it that can answer for a member
	// that never appeared.
	t.Logf("roster gate: %d/%d members covered (never_created=%d wrong_group=%d), orphans=%d splits=%d matched=%d created=%d queued=%d name_queued=%d status=%s (game version %s)",
		covered, len(exp.Members), len(neverCreated), len(wrongGroup), len(orphans), len(splits),
		res.Matched, res.Created, res.Queued, nameQueued, res.Status, exp.GameVersion)
}

// seedRosterCapture builds the database state one real roster capture would
// have left behind: an alliance, a screenshot row per frame pointing at the
// content-addressed blob, and a capture referencing them in scroll order.
//
// Members are deliberately NOT seeded. The VS gate seeds them because IngestVS
// never creates one; this route's entire job is to create them, and seeding
// would mean the gate measured nothing while reporting a number.
//
// The alliance is scoped to this run for the reason gate_test.go records:
// lw_manager_test is never truncated between runs, so a shared alliance
// accumulates every previous run's members. It also has to be the MOST
// RECENTLY observed alliance, because IngestRoster resolves its alliance via
// CurrentAllianceID, which is `ORDER BY observed_at DESC LIMIT 1`.
//
// complete=false: capture 1 is a partial capture and the fixture transcribes
// it as one. Condition 4 asserts that reconciliation says so, rather than
// asserting a status the capture never had.
func seedRosterCapture(t *testing.T, ctx context.Context, pool *db.Pool, exp expectedRoster) int64 {
	t.Helper()

	accountID := dbtest.SeedAccount(ctx, t, pool)

	if _, err := pool.UpsertAlliance(ctx, db.Alliance{
		Tag:         fmt.Sprintf("%s-%d", exp.Alliance.Tag, accountID),
		Name:        fmt.Sprintf("%s (roster gate run %d)", exp.Alliance.Name, accountID),
		MemberCount: exp.Alliance.MemberCount,
	}); err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
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
		frames[i] = db.CaptureFrameInput{
			ScreenshotID: shotID, Seq: f.Seq, OffsetPx: f.OffsetPx, GroupKey: f.GroupKey,
		}
	}

	if err := pool.RecordCapture(ctx, accountID, "roster", frames, false); err != nil {
		t.Fatalf("RecordCapture: %v", err)
	}

	var captureID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM captures WHERE account_id = $1 AND route = 'roster'`, accountID,
	).Scan(&captureID); err != nil {
		t.Fatalf("reading back the seeded capture: %v", err)
	}
	return captureID
}

// rosterGateBlobs opens the configured blob store and confirms every
// transcribed frame is in it. Only cfg.Blob is used: the database side goes
// through internal/dbtest, which reads LW_TEST_DATABASE_URL and never the
// application's LW_DATABASE_URL.
func rosterGateBlobs(t *testing.T, ctx context.Context, exp expectedRoster) blob.Store {
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
			t.Skipf("frame %d (%s) is not in the %s blob store; set LW_BLOB_FS_ROOT to an absolute path (see CLAUDE.md)",
				f.Seq, f.SHA256, cfg.Blob.Backend)
		}
	}
	return blobs
}
