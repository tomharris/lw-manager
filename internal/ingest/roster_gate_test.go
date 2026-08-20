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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

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
// R1 Danger Zone reads "0/12" and is still COLLAPSED in capture 1's final
// frame, so its twelve members have no rows anywhere in the capture: they are
// a group the capture saw and never opened, not twelve members the pipeline
// lost. The distinction is the whole reason this field exists -- without it a
// collapsed group is indistinguishable from a catastrophic read failure.
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

func TestRosterGateFixtureShape(t *testing.T) {
	exp := loadExpectedRoster(t, filepath.Join("testdata", "rostergate_shape.yaml"))
	if len(exp.Members) != 3 {
		t.Fatalf("shape fixture has %d members, want 3", len(exp.Members))
	}
	if exp.Groups[0].Total != 2 {
		t.Fatalf("group %q total = %d, want 2", exp.Groups[0].Rank, exp.Groups[0].Total)
	}
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
