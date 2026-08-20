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
	"os"
	"path/filepath"
	"sort"
	"testing"

	yaml "go.yaml.in/yaml/v3"
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
// misaligns every row after it. GroupKey is "_alliance_summary" on the one
// frame carrying the "Members: 96/100" line and empty on every member-list
// frame -- roster_capture deliberately asserts no group.
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
// generated.
//
// Every check below is a transcription mistake that would otherwise produce a
// confident, meaningless verdict.
func loadExpectedRoster(t *testing.T, path string) expectedRoster {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no hand-transcribed roster at %s; transcribe one first (see fixtures/m4rostergate/README.md)", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var exp expectedRoster
	if err := yaml.Unmarshal(data, &exp); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	switch {
	case exp.PeriodKey == "":
		t.Fatalf("%s: period_key is required", path)
	case exp.GameVersion == "":
		t.Fatalf("%s: game_version is required; it is what explains a gate that used to pass", path)
	case exp.Alliance.Tag == "" || exp.Alliance.Name == "":
		t.Fatalf("%s: alliance tag and name are required", path)
	case exp.Alliance.MemberCount <= 0:
		t.Fatalf("%s: alliance member_count is required; it is the alliance-total reconciliation's ground truth", path)
	case len(exp.Frames) == 0:
		t.Fatalf("%s: no frames listed", path)
	case len(exp.Groups) == 0:
		t.Fatalf("%s: no groups listed; group headers are ground truth here, not inferred", path)
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
			t.Fatalf("%s: a group has no rank", path)
		}
		if seenRank[g.Rank] {
			t.Fatalf("%s: rank %q appears twice; groups are keyed by rank", path, g.Rank)
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
			t.Fatalf("%s: group %q has total %d, want a positive size", path, g.Rank, g.Total)
		}
		sum += g.Total
	}
	if exp.Alliance.Leader == "" {
		t.Fatalf("%s: alliance leader is required; the sum check below is a statement about the leader having no group row", path)
	}
	if sum+1 != exp.Alliance.MemberCount {
		t.Fatalf("%s: group totals sum to %d, +1 for leader %q = %d, but the alliance frame reads %d; a header was misread, a group is missing, or this screen no longer puts exactly one member in the banner",
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
	for _, m := range exp.Members {
		if m.Name == "" {
			t.Fatalf("%s: a member in group %q has no name", path, m.Rank)
		}
		if !seenRank[m.Rank] {
			t.Fatalf("%s: member %q is in group %q, which is not in the groups list", path, m.Name, m.Rank)
		}
		if !expanded[m.Rank] {
			t.Fatalf("%s: member %q is in group %q, which this capture never expanded; there is no row to have read them from", path, m.Name, m.Rank)
		}
		if seenName[m.Name] {
			t.Fatalf("%s: %q is transcribed twice; the gate keys members by name, so a duplicate cannot be scored", path, m.Name)
		}
		seenName[m.Name] = true
	}

	sort.Slice(exp.Frames, func(i, j int) bool { return exp.Frames[i].Seq < exp.Frames[j].Seq })
	return exp
}
