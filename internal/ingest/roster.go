package ingest

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// memberListRegion and memberRowPitch mirror tasks.memberListRegion and
// tasks.memberRowPitch (measured on the handset — see internal/tasks/
// roster_capture.go). They are duplicated rather than imported: internal/
// tasks depends on internal/runtime, which this device-free package must
// not pull in, and capture_frames.offset_px was measured against exactly
// this region, so ingest re-measuring rows against a different one would
// silently misalign every row (see the package doc on SegmentRows/offset_px
// in the design). If the capture-side region ever moves, this constant must
// move with it.
//
// Y1 was 0.42 (y=672 of a 1600px frame), which sits inside the sticky
// rank-group header rather than below it — see groupHeaderRegion below for
// how that was measured and why 0.44 (y=704) is the corrected top.
var memberListRegion = transport.Rect{X1: 0.03, Y1: 0.44, X2: 0.97, Y2: 0.89}

const memberRowPitch = 112

// groupHeaderRegion is the band the sticky rank-group header ("R3 Footloose
// 15/64") occupies once it has pinned in place while rows scroll underneath
// it. It is its own measured constant, not derived from memberListRegion as
// it used to be: deriving Y2 as memberListRegion.Y1+0.05 meant moving the
// region's top dragged the header band down with it, off the header, and
// the derivation was already wrong before that move made it worse.
//
// Measured today, not estimated: for each row y across five separately
// scrolled frames of the same capture burst (docs/superpowers/specs/
// evidence/m4-scrolloffset-2026-08-13/), the mean RGB across the list's
// x-span (x=40..680 of the 720px-wide frame) was scanned for a sharp
// transition. All five frames agreed almost exactly — header bar top y=650
// (one frame read 651), bottom y=697 (all five) — which is itself the
// confirmation that the bar is genuinely pinned rather than coincidentally
// sitting there in one frame. Normalized against the 1600px frame that is
// 0.40625..0.435625.
//
// That measurement's own margin (widened to 0.404..0.438) is what task 21
// found too tall: the first real ingest read every header as noise, and
// TestPreprocMeasure (internal/vision/zz_preproc_probe_test.go) run against
// 18 real header crops from capture 1 (docs/superpowers/specs/evidence/
// m4-ocr-2026-08-14) showed why — at 0.404..0.438 the best preprocessing
// variant read the group name on only 10/18 frames, because the region's
// bottom edge reaches far enough into the next row that PSM 7 (single text
// line) merges the two (evidence Finding 2: a legible "Footloose" reading as
// "Se"). Tightening to 0.409..0.435 — still a margin around the measured
// 650..697 band, just a smaller one — raised that to 14/18 with the same
// preprocessing, the single biggest improvement task 21 measured on any
// field. It is not 18/18: the very first list frame of a capture (before any
// scroll) shows the header at its unscrolled position, not yet pinned, so
// that frame's header is expected to miss this band entirely rather than a
// preprocessing failure.
//
// The two constants are consistent by measurement, not by assumption:
// memberListRegion.Y1=0.44 is y=704, which clears this band's bottom edge
// (697) by 7px. Whoever moves either one next should keep that clearance.
var groupHeaderRegion = transport.Rect{X1: 0.03, Y1: 0.409, X2: 0.97, Y2: 0.435}

// groupHeaderOptions is the preprocessing task 21's harness measured for
// groupHeaderRegion: grayscale and upscale(3), nothing else. Across the same
// 18-frame set used to tighten the region above, every shape that included
// adaptive threshold after equalize scored 0-1/18 (equalize stretches the
// header's flat background before threshold amplifies that into noise — the
// flat-crop trap CLAUDE.md documents for NCC, in this algorithm); grayscale
// alone tied for the best score measured (14/18) without depending on that
// interaction, so it is the one used here rather than a threshold variant
// that happened to tie on this sample.
var groupHeaderOptions = vision.Options{SkipEqualize: true, SkipThreshold: true, SkipInvert: true, UpscaleFactor: 3}

// Field sub-rects, as fractions of the full frame width (X) and of one row
// band's own height (Y) — recon-measured from frame 03
// (docs/superpowers/specs/evidence/m4-recon-2026-08-12/03-r3-expanded-after-tap.png).
// Task 21 verified these against real capture-1 frames while measuring each
// field's Options below (ten real rows across two frames, cropped at exactly
// these fractions and read back by eye before OCR ever ran) — the crop
// geometry was correct; readField's missing per-field Options was not.
const (
	nameXFrac0, nameXFrac1     = 0.19, 0.67
	powerXFrac0, powerXFrac1   = 0.19, 0.47
	levelXFrac0, levelXFrac1   = 0.48, 0.67
	statusXFrac0, statusXFrac1 = 0.69, 0.97

	topRowYFrac0, topRowYFrac1       = 0.0, 0.50
	bottomRowYFrac0, bottomRowYFrac1 = 0.50, 1.0
	statusYFrac0, statusYFrac1       = 0.0, 0.45
)

// The MinConf values below are advisory except one: nameSpec's is enforced,
// at exactly one site.
//
// For the numeric fields nothing calls ocr.Result.Accepted(spec), so a read
// below its field's MinConf is neither rejected nor rerouted on that basis
// alone. The floor actually applied to them is the flat factConfidenceGate
// (0.80) in writeFacts, against each field's blended (name-match × OCR)
// confidence — which exceeds every value declared here, so this causes no
// wrong fact to be written. Those numbers are kept as documentation of each
// field's OCR difficulty (a free-text name at 0.4 versus a constrained-charset
// number at 0.6) and as the Charset each field is actually read with.
//
// The name is different in kind, which is why it is the exception. A numeric
// field's bad read costs one fact and is caught downstream by
// factConfidenceGate, which queues it for review. The name is not a fact —
// it is the identity every fact attaches to — so there is no downstream gate
// to catch it, and a bad read does not lose a number, it mints a *member*.
// processRow therefore enforces nameSpec.MinConf on the member-creation branch
// specifically (see the comment there for why creation and not matching, and
// for the measurement that says the floor costs nothing at 0.4).
// powerSpec carries no Charset, deliberately -- see task 23's report and
// parse.go's powerRe doc comment. It used to carry "0123456789.KMB", and
// that whitelist is exactly what Finding 7 (docs/superpowers/specs/evidence/
// m4-ocr-2026-08-14) found laundering a real row's raw text into a
// well-formed wrong value: "Power: 218.7M" fails unconstrained, correctly,
// but the whitelist recognized "1877M" instead, which parses to
// 1,877,000,000 -- 8.6x the true value. (Finding 7's writeup called this "a
// confident wrong fact"; task 23's fix-round re-check found every one of the
// 33 laundered reads scored OCR confidence 0.00 under the shipped Options,
// so factConfidenceGate would have queued all of them as
// low_confidence_power rather than writing a fact -- participation_facts
// held zero power rows before this fix. The defect is not that a wrong
// number reached a leaderboard; it is that ParsePower called a laundered
// string well-formed at all, which means a human reviewer sees the plausible
// "1877M" instead of the visibly-broken "Power:je18°7M" it actually was, and
// that a future confidence improvement on this field would have had nothing
// left to catch it. Both are still real; neither is "corrupted the facts
// table".) Task 23 re-measured the parse-shape question against 53 real
// member rows from capture 1 rather than trusting the one example: with the
// whitelist, 33/53 (62%) parsed to a value 10x-1000x off; without it, 0/53
// false accepted -- every unconstrained read either matched the true value
// or failed ParsePower's regex outright, which is the "lose the row rather
// than record a plausible wrong value" trade CLAUDE.md's invariant #5 asks
// for regardless of which gate would also have caught it. The general rule
// (put here because a whitelist reads as an obvious accuracy win to whoever
// finds this code next): a charset is safe only where every character it
// removes would also be absent from a correct read. A correct
// "Power: 218.7M" contains "P","o","w","e","r",":"," " -- none of them in
// "0123456789.KMB" -- so the whitelist was stripping characters a correct
// read legitimately has, and constraining tesseract's classifier to a
// charset does not just filter recognized text, it changes what each glyph
// is classified as; the decimal point was the measured casualty even though
// "." was itself in the allowed set. See vs.go's vsPointsSpec for the same
// rule applied to a charset that looked safe by a superficially similar
// argument and was not (task 23 fix-round finding C1) -- "the charset only
// contains characters a correct read has" is necessary but not sufficient;
// what also matters is whether the *crop* can contain other real content
// (points' crop catches the alliance-name line below; power's catches the
// "Power:" label) that the charset then has nowhere to put.
var (
	groupHeaderSpec = ocr.Spec{MinConf: 0.5}
	nameSpec        = ocr.Spec{MinConf: 0.4}
	powerSpec       = ocr.Spec{MinConf: 0.6}

	// levelSpec's charset is kept, on a narrower basis than a first pass at
	// this comment claimed. "0/53 disagreements" is not, by itself, evidence
	// of anything: 0/53 unconstrained level reads parse at all (this field's
	// crop is small enough that the "L" glyph reads as "Y" or "E" without the
	// charset's help), so there was no row on which the two conditions could
	// even disagree. The whitelist does 100% of this field's parsing (6/53
	// rows recovered). The actual basis for trusting it: in every one of
	// those 6 rescues, the *digits* the unconstrained read already carried
	// were correct and untouched -- "Lyi34"->"Lv34", "Evi35"->"Lv35" -- the
	// whitelist only ever repaired the "L"/"v" glyphs, never the digits that
	// determine the parsed value, because "Lv.0123456789" is a superset of
	// what a correct read is built from and there is no other field's
	// content sharing this crop for it to launder in from (unlike points --
	// see powerSpec's comment above). A cross-field probe (this charset run
	// over the 53 power and last-active crops instead of level crops)
	// fabricated no "Lv##" out of either field's content, which is the
	// closest this measurement gets to ruling out the C1 failure mode
	// directly rather than by argument.
	levelSpec = ocr.Spec{Charset: "Lv.0123456789", MinConf: 0.6}

	// lastActiveSpec's charset is kept on the same structural grounds as
	// levelSpec: "0123456789hmdagoOnline " covers every character a correct
	// read contains ("7h ago", "23m ago", "1d ago", "Online"), so nothing it
	// strips could have been part of a valid read. Measured against the same
	// 53 real rows, the whitelisted and unconstrained reads parsed to the
	// identical value on every single row that parsed at all (34/53 both
	// ways, zero disagreements) -- the whitelist did not launder a single
	// row, and did not rescue one either.
	lastActiveSpec = ocr.Spec{Charset: "0123456789hmdagoOnline ", MinConf: 0.6}
)

// Per-field preprocessing Options, set by TestPreprocMeasure against ten
// real member rows (five each from two capture-1 frames, cross-checked by
// eye against the frame before scoring — docs/superpowers/specs/evidence/
// m4-ocr-2026-08-14). readField used to receive only Region, so every field
// silently got the full Preprocess chain (see preprocess.go's doc comment
// for why that chain is wrong on this UI); these are what measurement on
// real pixels put in its place.
var (
	// nameOptions: grayscale + invert + upscale(2) read 9/10 real names
	// (e.g. "KIRCHO", "BobLeeSwagger44"). Threshold tied at 9/10 too but
	// equalize did not (5/10) — see powerOptions below for why equalize
	// specifically helps the power field and not this one: player names
	// render as flat-colored text on a flat card background, the same
	// near-flat condition that makes threshold and equalize risky
	// elsewhere, so this keeps both off and relies on grayscale contrast
	// alone plus the polarity flip.
	nameOptions = vision.Options{SkipEqualize: true, SkipThreshold: true, UpscaleFactor: 2}

	// powerOptions: grayscale + equalize + upscale(3) was the best measured
	// (2/10 exact "###.#M" reads, tied with the full chain), and every
	// variant tested lost the decimal point on most rows regardless of
	// shape ("184.3M" read as "1843M") — a 10x-magnitude error that
	// parses without complaint, since powerRe's decimal is optional. This
	// is a real, measured limit of grayscale-based OCR on this field's bold
	// condensed digits, not a shape/upscale choice; the confidence gate is
	// what stands between a dropped decimal and a wrong fact today, and a
	// follow-up task should treat this field's low ceiling as a finding, not
	// as evidence the wrong Options were picked.
	powerOptions = vision.Options{SkipThreshold: true, SkipInvert: true, UpscaleFactor: 3}

	// levelOptions: grayscale + upscale(3) read 10/10 real levels ("Lv.35"
	// and friends) — level is short, high-contrast, and constrained to a
	// small charset, so it was the easiest field measured and several
	// shapes tied; grayscale-only was chosen for consistency with the other
	// fields that also tied on it, not because the alternatives were worse.
	levelOptions = vision.Options{SkipEqualize: true, SkipThreshold: true, SkipInvert: true, UpscaleFactor: 3}

	// lastActiveOptions: grayscale + upscale(3) read 5/10 — every "Xm ago"/
	// "Xh ago" row (grey text) read cleanly, and every "Online" row (green
	// text) read as garbage ("oo", "ae") under every shape tested,
	// including ones with equalize. Grayscale conversion maps that green
	// close to the card's own background luminance, so the text is not
	// faint in the source, it is nearly invisible once color is discarded —
	// a different mechanism from the flat-region trap above but the same
	// shape of problem: a normalizing/reducing step erasing the one signal
	// the field depends on. No Options combination this package exposes
	// fixes it; a color-aware read (or a fixed "online" glyph match) is a
	// separate task's problem, not a threshold this one got wrong.
	lastActiveOptions = vision.Options{SkipEqualize: true, SkipThreshold: true, SkipInvert: true, UpscaleFactor: 3}
)

// allianceMemberCountRegion is the band on the alliance screen (not
// alliance_members) carrying "Members: 96/100" — recon frame 01
// (docs/superpowers/specs/evidence/m4-recon-2026-08-12/01-alliance-members-96-of-100.png).
//
// Task 21 found two faults here, both against real frames (this recon frame
// and capture 1's own alliance-summary screenshot, seq 0): X2=0.60 (x=432)
// cut off the value entirely — "Members:" sits at x≈270..390 but its value
// "97/100" sits at x≈600..680 — which is why the first real run warned with
// raw_text="4 ES". Widening X2 alone is not sufficient, though: the old Y
// span (0.19..0.27, y=304..432) is tall enough to also catch the "Power:"
// line above and the "Language:" line below, so PSM 7 would read three
// concatenated lines instead of one. Y is now tightened to the "Members:"
// line alone (measured the same way as groupHeaderRegion, a lum-transition
// scan of the real frame: text band y=347..383 of a 1600px frame).
// TestPreprocMeasure against both real frames confirms the fix: the shipped
// chain (full Preprocess, old region) read neither cleanly; grayscale +
// upscale(3) at this region reads "Members: 97/100" and "Members: 96/100"
// exactly, 2/2.
var allianceMemberCountRegion = transport.Rect{X1: 0.03, Y1: 0.2169, X2: 0.97, Y2: 0.2394}

// See roster.go's Spec-value doc comment above groupHeaderSpec: advisory,
// not enforced — factConfidenceGate is not applied to this read at all,
// since it never becomes a Fact; a failed read just degrades the
// alliance-total check (see readAllianceMemberCount).
var allianceMemberCountSpec = ocr.Spec{MinConf: 0.5}

// allianceMemberCountOptions: grayscale + upscale(3), the same shape that
// won on every other field task 21 measured — see allianceMemberCountRegion
// above for the 2/2 result this and the region fix together produced.
var allianceMemberCountOptions = vision.Options{SkipEqualize: true, SkipThreshold: true, SkipInvert: true, UpscaleFactor: 3}

// allianceMemberCountRe pulls the alliance's current member count out of the
// alliance screen's "Members: 96/100" line. Only the first number is the
// roster's reconciliation ground truth — the second is the alliance's
// capacity, not a headcount (recon: "R5 1 + R4 9 + R3 64 + R2 11 + R1 11 =
// 96 = Members: 96/100").
var allianceMemberCountRe = regexp.MustCompile(`Members:\s*(\d+)\s*/\s*\d+`)

// parseAllianceMemberCount extracts the alliance's current member count from
// the alliance frame's raw OCR text. An unparseable read means the
// alliance-total check cannot run this pass, not that the alliance has zero
// members — callers must treat the returned error as "unavailable", never
// substitute a zero (see readAllianceMemberCount).
func parseAllianceMemberCount(raw string) (int, error) {
	m := allianceMemberCountRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, fmt.Errorf("ingest: alliance member count %q: %w", raw, ErrUnparseable)
	}
	count, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("ingest: alliance member count %q: %w", raw, ErrUnparseable)
	}
	return count, nil
}

// RosterRow is one parsed member-list row.
type RosterRow struct {
	Name            string
	NameConf        float64
	Power           int64
	Level           int
	LastActiveHours float64
	ScreenshotID    int64
	Band            RowBand
	GroupKey        string
}

// GroupTally is one rank group's row-count reconciliation: how many members
// the group's own sticky header claims against how many rows ingest actually
// collected there, via geometric dedupe. It is a count of rows, not of
// members successfully matched or created — a row that failed to parse or
// was queued for review still counts here, because reconciliation exists to
// catch rows that were never photographed at all, and an OCR failure further
// downstream must not be confused with that.
type GroupTally struct {
	Expected, Parsed int

	// Name is the group's display name as read off its own sticky header
	// (parseGroupHeader), taken from whichever frame first established this
	// rank's tally. It is descriptive only — printRosterSummary surfaces it
	// so a human doing review triage can tell "R3" from "Footloose" without
	// opening a screenshot — and must never be used as a key: group names
	// are user-editable and the group set itself varies release to release
	// (CLAUDE.md, "Rank groups have no fixed identity"), which is exactly
	// why GroupTally is keyed on rank (matchRankBadge's NCC read) and not on
	// this field.
	Name string
}

// RosterResult summarizes one IngestRoster run.
type RosterResult struct {
	Matched, Created, Queued int
	PerGroup                 map[string]GroupTally
	Status                   string

	// AllianceMemberCount is the "96" read from the alliance frame's
	// "Members: 96/100" line — the same number written to
	// alliances.member_count. Zero when AllianceTotalChecked is false.
	AllianceMemberCount int
	// AllianceTotalChecked reports whether the sum of every group's parsed
	// row count was reconciled against AllianceMemberCount this run. False
	// means the alliance frame was missing from the capture, or present but
	// unreadable — the alliance-total check is simply unavailable in that
	// case, and Status reflects per-group reconciliation alone, exactly as
	// it did before this check existed. Losing a whole roster capture to one
	// frame's bad OCR would cost more than the check being unavailable.
	AllianceTotalChecked bool
}

// rosterRun carries the state one IngestRoster call accumulates across
// frames: which alliance, which members are known so far (grown in place as
// rows create new ones), and each group's expected total and dedupe cursor.
type rosterRun struct {
	captureID  int64
	allianceID int64
	members    []roster.Member
	groups     map[string]*groupTracker
	observedAt time.Time
	periodKey  string
	res        RosterResult
}

// groupTracker is one rank group's running state: its header-stated total,
// how many rows have resolved to a member (matched or created — this is what
// "group full" is checked against, per the recon's structural guard), and
// the geometric dedupe cursor.
type groupTracker struct {
	expected         int
	matchedOrCreated int
	contentY         int
	lastRowY         int // content-Y of the last collected row; -1 = none yet
}

// advance decides this frame's contentY and whether its topmost detected
// band must be discarded as the sticky header's occlusion of an already-
// collected row (see the call site's own comment on that band-drop, and
// TestIngestRosterDiscardsTheOccludedTopRow). sameGroupAsPrevFrame is true
// only when the frame immediately before this one — in capture order, not
// in "frames of this group" order — carried this same group's header: i.e.
// this frame is an unbroken continuation of a scroll that was already moving
// through this group when the previous frame was captured.
//
// frame.OffsetPx is the scroll distance between two CONSECUTIVE frames of
// the capture, not "distance this group's list has moved since its tracker
// was last touched" — those coincide only when the previous frame belonged
// to this same group. Capture 1 (task 27's evidence, finding 10) shows why
// that distinction is load-bearing: a device with two rank groups expanded
// at once can carry frame N's header for R3 and frame N+1's for R2 while a
// single continuous swipe is in progress, so the pixels moved between them
// reflect whatever was on screen during that swipe, not R3's list specifically.
// Attributing that distance to R3 (the bug this replaces) or to R2 would
// both be guessing at a quantity this frame pair does not carry evidence
// for. So a group switch of EITHER kind — a group's first-ever frame, or a
// group returned to after one or more frames of a DIFFERENT group in
// between — leaves contentY exactly where this tracker last put it: 0 if
// this group has never been seen before, or its last accumulated value if
// resuming one already in progress. That is a deliberate claim that an
// away group does not move while it is off screen, which is the only
// assumption this frame pair has any evidence for either way — and it is
// what makes returning to a group idempotent rather than merely
// non-crashing: the resumed position lines up with gt.lastRowY well enough
// for the geometric dedupe below to recognize rows already collected,
// instead of a reset making them look brand new (task 27's brief; the
// resulting duplicate INSERT is what actually crashed the first real run).
func (gt *groupTracker) advance(offsetPx int, sameGroupAsPrevFrame bool) (contentY int, skipTopBand bool) {
	if sameGroupAsPrevFrame {
		gt.contentY += offsetPx
		return gt.contentY, true
	}
	return gt.contentY, false
}

// IngestRoster turns one roster capture's frames into members and facts.
//
// Rank is not supplied by roster_capture — capture_frames.group_key arrives
// empty on every member-list frame, deliberately (see the M4 task
// amendments): group names are user-edited and the group set varies, so the
// capture task cannot assert which group it is looking at. Every frame
// carries its own evidence instead, in its sticky group header, which this
// function OCRs on every frame rather than trusting a label asserted
// elsewhere.
//
// One frame is the deliberate exception: the alliance screen's summary
// frame, tagged vision.AllianceSummaryGroupKey by roster_capture, is not a
// list frame at all. It is pulled out before the member-list loop below,
// read for its own "Members: 96/100" total (readAllianceMemberCount), and
// never handed to SegmentRows — running row segmentation over it would
// produce garbage bands.
//
// periodKey is supplied by the caller rather than computed here, matching
// IngestVS's shape — both routes take it as an explicit argument so the
// caller (cmd/control's `ingest` command) derives it once, from the
// capture's own started_at, and neither route can silently fall back to
// wall-clock now. observedAt (the facts' own timestamp) is likewise pinned
// to the capture's started_at rather than time.Now(): a parser fix replayed
// over a capture from months ago must write the same facts it would have
// written the day the screenshot was taken, and stamping "now" on either
// value would defeat that.
func (i *Ingester) IngestRoster(ctx context.Context, captureID int64, periodKey string) (RosterResult, error) {
	capture, err := i.store.Capture(ctx, captureID)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: loading capture %d: %w", captureID, err)
	}
	frames, err := i.store.CaptureFrames(ctx, captureID)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: loading frames for capture %d: %w", captureID, err)
	}

	// CurrentAllianceID's own ErrNotFound means literally nothing has ever
	// written the alliances table -- the state of a fresh deployment, since
	// alliance identity is declared via `control alliance set`, never
	// derived from the roster frame's own pixels (see that command's own
	// doc comment for why). Left as CurrentAllianceID's bare wrap, the
	// failure read as "db: current alliance: db: not found" with nothing
	// telling the operator what to do about it -- this is that fix, named
	// here rather than in CurrentAllianceID itself so the sentinel stays
	// wrapped with %w and errors.Is(err, db.ErrNotFound) keeps working.
	allianceID, err := i.store.CurrentAllianceID(ctx)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: resolving current alliance (run `control alliance set --tag <tag> --name <name>` first): %w", err)
	}
	dbMembers, err := i.store.ListMembers(ctx, allianceID)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: listing members for alliance %d: %w", allianceID, err)
	}
	aliases, err := i.store.MemberAliases(ctx, allianceID)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: listing aliases for alliance %d: %w", allianceID, err)
	}

	run := &rosterRun{
		captureID:  captureID,
		allianceID: allianceID,
		members:    toRosterMembers(dbMembers, aliases),
		groups:     map[string]*groupTracker{},
		observedAt: capture.StartedAt,
		periodKey:  periodKey,
		res:        RosterResult{PerGroup: map[string]GroupTally{}},
	}

	// The alliance frame (vision.AllianceSummaryGroupKey) is not a list
	// screen and must never reach row segmentation — pull it out before the
	// main loop rather than let the loop special-case it inline, so the
	// newGroup/prevGroupKey bookkeeping below, which assumes every frame it
	// sees is a scrolled member-list frame, never has to know this one
	// exists. It is expected to be missing from captures recorded before
	// this check existed, and IngestRoster must still ingest those exactly
	// as before.
	var allianceFrame *db.CaptureFrame
	listFrames := make([]db.CaptureFrame, 0, len(frames))
	for idx := range frames {
		if frames[idx].GroupKey == vision.AllianceSummaryGroupKey {
			f := frames[idx]
			allianceFrame = &f
			continue
		}
		listFrames = append(listFrames, frames[idx])
	}

	if allianceFrame != nil {
		if count, ok := run.readAllianceMemberCount(ctx, i, *allianceFrame); ok {
			run.res.AllianceMemberCount = count
			run.res.AllianceTotalChecked = true
		}
	}

	var prevGroupKey string
	havePrev := false
	var totalParsed int

	for _, frame := range listFrames {
		img, err := i.loadFrame(ctx, frame.ScreenshotID)
		if err != nil {
			return RosterResult{}, fmt.Errorf("ingest: loading screenshot %d: %w", frame.ScreenshotID, err)
		}

		headerRes, err := i.readField(ctx, img, groupHeaderRegion, groupHeaderSpec, groupHeaderOptions)
		if err != nil {
			return RosterResult{}, fmt.Errorf("ingest: reading group header on screenshot %d: %w", frame.ScreenshotID, err)
		}
		hy0 := int(groupHeaderRegion.Y1 * float64(img.Bounds().Dy()))
		hy1 := int(groupHeaderRegion.Y2 * float64(img.Bounds().Dy()))

		// The count comes from OCR (it reads cleanly — task 24's brief); the
		// rank does not (Finding 4: outlined game glyphs do not OCR under any
		// PSM or charset tried). The two reads are independent and both must
		// succeed before this frame's rows can be attributed to a group, so
		// each gets its own failure path to its own review reason rather than
		// one collapsing into the other's error message.
		groupName, headerTotal, herr := parseGroupHeader(headerRes.Text)
		if herr != nil {
			if err := run.queueReview(ctx, i, frame.ScreenshotID, RowBand{Y0: hy0, Y1: hy1}, headerRes.Text, nil, "unparseable_group_header", 0); err != nil {
				return RosterResult{}, err
			}
			continue
		}
		rankRes, rerr := matchRankBadge(img)
		if rerr != nil {
			// Only an unconvincing *match* is review-worthy. Any other error
			// out of matchRankBadge means the binary itself is broken -- a
			// template that failed to embed or decode, which loadRankTemplates'
			// own doc comment already distinguishes from "one frame's rank is
			// unreadable" and which sync.Once makes permanent for the process.
			// Treating that as a per-frame review row would turn a broken
			// build into a capture-sized pile of human work reported as
			// status "partial", which is the loudest possible way to say
			// nothing. It fails the run instead.
			if !errors.Is(rerr, ErrNoConfidentRank) {
				return RosterResult{}, fmt.Errorf("ingest: capture %d frame seq %d: matching rank badge: %w", captureID, frame.Seq, rerr)
			}
			// A badge matching nothing with enough confidence is exactly the
			// case CLAUDE.md invariant #3 forbids acting on: this frame's
			// rows would have nowhere honest to attach (see this file's
			// package doc on rank not being supplied by roster_capture), so
			// the whole frame goes to review rather than guessing which
			// rank group is on screen. headerRes.Text rides along on the
			// review row so a human sees the same count the OCR side
			// already resolved, not just "something didn't match."
			if err := run.queueReview(ctx, i, frame.ScreenshotID, RowBand{Y0: hy0, Y1: hy1}, headerRes.Text, nil, "unmatched_rank_badge", 0); err != nil {
				return RosterResult{}, err
			}
			continue
		}
		groupKey := rankRes.Rank
		// Rank is not OCR-derived, so invariant #5's confidence-on-every-fact
		// rule does not literally reach it and members.Rank has nowhere to
		// carry a score. Its provenance is still worth having when a capture
		// is being argued about after the fact -- every frame's rank decision
		// with the numbers that produced it, against the screenshot it came
		// from. Logged rather than stored: a schema column for it would need
		// a migration and a supersession story of its own, and nothing has
		// asked to query these yet.
		slog.DebugContext(ctx, "ingest: frame rank matched",
			"capture_id", captureID, "frame_seq", frame.Seq, "screenshot_id", frame.ScreenshotID,
			"rank", rankRes.Rank, "score", rankRes.Score, "gap", rankRes.Gap)

		gt, exists := run.groups[groupKey]
		if !exists {
			gt = &groupTracker{expected: headerTotal, lastRowY: -1}
			run.groups[groupKey] = gt
			run.res.PerGroup[groupKey] = GroupTally{Expected: headerTotal, Name: groupName}
		}

		sameGroupAsPrevFrame := havePrev && groupKey == prevGroupKey
		_, skipTopBand := gt.advance(frame.OffsetPx, sameGroupAsPrevFrame)
		prevGroupKey, havePrev = groupKey, true

		bands, err := SegmentRows(img, memberListRegion, memberRowPitch)
		if err != nil {
			return RosterResult{}, fmt.Errorf("ingest: segmenting screenshot %d: %w", frame.ScreenshotID, err)
		}
		if skipTopBand && len(bands) > 0 {
			// memberListRegion.Y1 is a fixed pixel line (704, 7px below the
			// sticky header's own bottom edge — see memberListRegion's doc
			// comment) and the list keeps scrolling underneath it, so this is
			// no longer "the header covers the first band" as it was
			// described here before the region moved to clear the header:
			// with Y1 below the header, the header cannot be why a band gets
			// cut. It is still true, for an unrelated reason — a swipe's
			// travel is essentially never an exact multiple of a row's
			// pitch, so the fixed region-top line almost always bisects
			// whichever row happens to be sitting across it once the list
			// has moved at all within the same group (skipTopBand is true).
			// The result looks identical either way (a partial top band
			// that must not be parsed as a whole row), which is exactly why
			// the wrong reason survived a region move undetected — see
			// CLAUDE.md on the `vs` mislabel for the general shape of that
			// failure. Discard it rather than parse a partial row.
			bands = bands[1:]
		}

		regionTop := int(memberListRegion.Y1 * float64(img.Bounds().Dy()))
		for _, band := range bands {
			rowY := gt.contentY + (band.Y0 - regionTop)
			// The design doc also specifies an identity-based cross-check
			// alongside this geometric dedupe, flagging a disagreement
			// between the two counts. It is deliberately not implemented
			// here. That check exists to catch a member's row appearing at
			// two different screen positions at once — which geometric
			// dedupe structurally cannot detect, since it only compares a
			// row's position to the previous one — and the only place recon
			// observed that phenomenon is the VS ranking's pinned self row
			// (docs/superpowers/specs/2026-08-12-m4-recon-findings.md §2:
			// "the self row is pinned... and also appears in its natural
			// position"). Recon's roster notes (§1) record no such
			// duplicate: the logged-in account's row is unremarkable except
			// for lacking a Manage button. A pinned row is a property of the
			// VS ranking screen, not of alliance_members, so the identity
			// cross-check belongs to the VS ingest path, not here.
			if gt.lastRowY >= 0 && rowY <= gt.lastRowY+memberRowPitch/2 {
				continue // geometric duplicate; OCR never runs on it
			}
			gt.lastRowY = rowY

			totalParsed++
			tally := run.res.PerGroup[groupKey]
			tally.Parsed++
			run.res.PerGroup[groupKey] = tally

			if err := run.processRow(ctx, i, img, band, frame.ScreenshotID, groupKey); err != nil {
				return RosterResult{}, err
			}
		}
	}

	status := "complete"
	for _, t := range run.res.PerGroup {
		if t.Parsed != t.Expected {
			status = "partial"
			break
		}
	}

	// The alliance-total check is additional to per-group reconciliation
	// above, not a replacement for it: it catches what per-group checks
	// structurally cannot, since it sums what was actually parsed rather
	// than depending on every group having been seen at all. A group whose
	// frames never made it into this capture leaves no PerGroup entry to
	// fall short — the loop above would call that "complete" — but the sum
	// of every group that *was* seen still falls short of the alliance's own
	// count, and only this check catches it.
	if run.res.AllianceTotalChecked && totalParsed != run.res.AllianceMemberCount {
		status = "partial"
		slog.WarnContext(ctx, "ingest: roster alliance-total reconciliation found a mismatch",
			"capture_id", captureID, "parsed_total", totalParsed, "alliance_member_count", run.res.AllianceMemberCount)
	}
	run.res.Status = status

	// Recorded whenever the read succeeded, independent of whether it
	// reconciled — the alliance screen's own count is worth keeping even
	// when the parsed rows do not add up to it, per the M4 task-11b brief
	// ("populate alliances.member_count from the same read").
	if run.res.AllianceTotalChecked {
		if err := i.store.SetAllianceMemberCount(ctx, allianceID, run.res.AllianceMemberCount); err != nil {
			return RosterResult{}, fmt.Errorf("ingest: recording alliance %d member count: %w", allianceID, err)
		}
	}

	if err := i.store.FinishCapture(ctx, captureID, status, totalParsed, ""); err != nil {
		return RosterResult{}, fmt.Errorf("ingest: finishing capture %d: %w", captureID, err)
	}
	return run.res, nil
}

// readAllianceMemberCount OCRs and parses the alliance frame's
// "Members: 96/100" line. Any failure — the blob missing, the OCR engine
// erroring, or the text not matching the expected shape — degrades to
// "unavailable" (ok == false) rather than propagating: per the M4 task-11b
// brief, losing an entire otherwise-good roster capture to one frame's bad
// OCR would cost more than the alliance-total check simply not running this
// pass. IngestRoster falls back to per-group reconciliation alone in that
// case, exactly as it did before this check existed.
func (run *rosterRun) readAllianceMemberCount(ctx context.Context, i *Ingester, frame db.CaptureFrame) (int, bool) {
	img, err := i.loadFrame(ctx, frame.ScreenshotID)
	if err != nil {
		slog.WarnContext(ctx, "ingest: could not load the alliance frame; alliance-total reconciliation unavailable this run",
			"capture_id", run.captureID, "screenshot_id", frame.ScreenshotID, "error", err)
		return 0, false
	}
	res, err := i.readField(ctx, img, allianceMemberCountRegion, allianceMemberCountSpec, allianceMemberCountOptions)
	if err != nil {
		slog.WarnContext(ctx, "ingest: could not OCR the alliance frame's member count; alliance-total reconciliation unavailable this run",
			"capture_id", run.captureID, "screenshot_id", frame.ScreenshotID, "error", err)
		return 0, false
	}
	count, err := parseAllianceMemberCount(res.Text)
	if err != nil {
		slog.WarnContext(ctx, "ingest: could not parse the alliance frame's member count; alliance-total reconciliation unavailable this run",
			"capture_id", run.captureID, "screenshot_id", frame.ScreenshotID, "raw_text", res.Text)
		return 0, false
	}
	return count, true
}

// processRow crops and reads one row's four fields, then routes it to a
// match, a creation, or the review queue.
//
// Name resolution and numeric-field parsing are independent: a row's three
// numeric fields are parsed but not gated on one another, and matching the
// name proceeds regardless of what happened to power/level/last-active.
// Facts are per-metric (the M4 amendment on this task), so an unparseable
// power field must not cost a row its perfectly good level and
// last_active_hours — only that one field is queued for review, and
// writeFacts is what makes that split per-field rather than per-row.
//
// The one thing numeric parsing cannot survive is a name that fails to
// resolve to a member at all: without a memberID no fact has anywhere to
// attach, so an ambiguous or unmatched-and-group-full name still sends the
// whole row to review, same as before.
func (run *rosterRun) processRow(ctx context.Context, i *Ingester, img image.Image, band RowBand, screenshotID int64, groupKey string) error {
	nameRes, err := i.readField(ctx, img, fieldRect(band, img, nameXFrac0, nameXFrac1, topRowYFrac0, topRowYFrac1), nameSpec, nameOptions)
	if err != nil {
		return err
	}
	powerRes, err := i.readField(ctx, img, fieldRect(band, img, powerXFrac0, powerXFrac1, bottomRowYFrac0, bottomRowYFrac1), powerSpec, powerOptions)
	if err != nil {
		return err
	}
	levelRes, err := i.readField(ctx, img, fieldRect(band, img, levelXFrac0, levelXFrac1, bottomRowYFrac0, bottomRowYFrac1), levelSpec, levelOptions)
	if err != nil {
		return err
	}
	lastRes, err := i.readField(ctx, img, fieldRect(band, img, statusXFrac0, statusXFrac1, statusYFrac0, statusYFrac1), lastActiveSpec, lastActiveOptions)
	if err != nil {
		return err
	}

	power, perr := ParsePower(powerRes.Text)
	level, lerr := ParseLevel(levelRes.Text)
	lastActive, aerr := ParseLastActiveHours(lastRes.Text)

	row := RosterRow{
		Name: nameRes.Text, NameConf: nameRes.Confidence,
		Power: power, Level: level, LastActiveHours: lastActive,
		ScreenshotID: screenshotID, Band: band, GroupKey: groupKey,
	}

	// An empty name cannot identify anybody, and it cannot be matched either:
	// roster.Rank("") scores 0 against every member, so without this the row
	// falls through to the creation branch below and mints a member named "".
	// That is not a cosmetic defect -- each one consumes a slot of the group's
	// creation budget (gt.matchedOrCreated below), displacing a real member
	// into no_confident_match_group_full, and it accumulates on every re-run
	// because an empty name does not match the empty-named member the last run
	// created. Capture 1 produced 20 such rows per run and 122 ghost members
	// before this check existed.
	//
	// Structural, not a threshold: this holds whatever confidence the engine
	// reports, because there is no reading of an empty string that identifies
	// a person. (On capture 1 all 20 also scored exactly 0.000, so the
	// confidence gate below would have caught them too -- the point of keeping
	// both is that neither should have to rely on the other.)
	if strings.TrimSpace(row.Name) == "" {
		return run.queueReview(ctx, i, screenshotID, band, nameRes.Text, nil, "unreadable_name", row.NameConf)
	}

	candidates := roster.Rank(row.Name, run.members)
	switch {
	case len(candidates) > 0 && candidates[0].Score >= roster.AutoAccept:
		gt := run.groups[groupKey]
		gt.matchedOrCreated++
		run.res.Matched++
		matchNorm := float64(candidates[0].Score) / 100.0
		return run.writeFacts(ctx, i, screenshotID, band, candidates[0].MemberID, matchNorm, fieldReads(row, powerRes, levelRes, lastRes, perr, lerr, aerr))

	case len(candidates) > 0 && candidates[0].Score >= roster.ReviewFloor:
		return run.queueReview(ctx, i, screenshotID, band, row.Name, candidates, "ambiguous_name_match", 0)

	default:
		// Creating a member is the one irreversible thing this loop does: a
		// match writes a fact against somebody who already exists, but a
		// creation mints an identity every later capture will try to match
		// against. So it is the one place nameSpec.MinConf is enforced rather
		// than advisory (see the specs' own doc comment above).
		//
		// Only here, and deliberately not on the matching branches. A fuzzy
		// score of 92+ against a known member is far stronger evidence that a
		// read is right than the OCR engine's own confidence is -- on capture 1
		// seven rows scored below 0.4 and still auto-matched a real member, and
		// refusing those would lose good data to no purpose.
		//
		// Measured before it was enforced: across capture 1's 96 rows, no
		// non-empty name below this floor reached the creation branch at all
		// (every sub-0.4 read either matched at 92+ or was empty), so turning
		// this on costs zero legitimate creations on the only real roster
		// capture there is. That makes it a guard against a class rather than
		// a threshold tuned to trim a distribution -- if a future capture
		// starts losing real members here, the answer is to re-measure the
		// field's OCR, not to lower this.
		if !nameRes.Accepted(nameSpec) {
			return run.queueReview(ctx, i, screenshotID, band, nameRes.Text, candidates, "low_confidence_name", row.NameConf)
		}
		gt := run.groups[groupKey]
		if gt.matchedOrCreated < gt.expected {
			memberID, err := i.store.CreateMember(ctx, db.Member{
				AllianceID:     run.allianceID,
				Name:           row.Name,
				NameNormalized: roster.Normalize(row.Name),
				Rank:           groupKey,
			})
			if err != nil {
				return fmt.Errorf("ingest: creating member %q: %w", row.Name, err)
			}
			run.members = append(run.members, roster.Member{ID: memberID, Name: row.Name})
			gt.matchedOrCreated++
			run.res.Created++
			// A newly created member has no candidate to score against, so
			// its match component is 1.0: the row is definitionally this
			// member. Only each field's own OCR confidence gates its fact
			// from here.
			return run.writeFacts(ctx, i, screenshotID, band, memberID, 1.0, fieldReads(row, powerRes, levelRes, lastRes, perr, lerr, aerr))
		}
		// The group's own header says it is already full. A 12th "new
		// member" against an 11-member group is an OCR artifact, not a
		// person — the recon's structural guard, not a tuned threshold.
		return run.queueReview(ctx, i, screenshotID, band, row.Name, candidates, "no_confident_match_group_full", 0)
	}
}

// fieldRead is one numeric field's OCR read, parse outcome, and the label
// used to name it in a review row's reason ("power", "level",
// "last_active") — kept distinct from Fact.Metric ("last_active_hours")
// so a reviewer sees the same word the field crop is named after.
type fieldRead struct {
	metric, label string
	value, conf   float64
	raw           string
	err           error
}

// fieldReads assembles the three numeric fields' reads for writeFacts, in a
// fixed order so a partially-failed row always reports power, then level,
// then last-active, regardless of which one failed.
func fieldReads(row RosterRow, powerRes, levelRes, lastRes ocr.Result, perr, lerr, aerr error) [3]fieldRead {
	return [3]fieldRead{
		{metric: "power", label: "power", value: float64(row.Power), conf: powerRes.Confidence, raw: powerRes.Text, err: perr},
		{metric: "level", label: "level", value: float64(row.Level), conf: levelRes.Confidence, raw: levelRes.Text, err: lerr},
		{metric: "last_active_hours", label: "last_active", value: row.LastActiveHours, conf: lastRes.Confidence, raw: lastRes.Text, err: aerr},
	}
}

// factConfidenceGate is the floor CLAUDE.md invariant #5 sets: "low-confidence
// reads go to the review queue, never to a leaderboard." Facts are exactly
// what a leaderboard reads, so the gate is enforced here, at write time, not
// as a filter a later query is trusted to remember — the first query that
// forgets would otherwise put an unverifiable number in front of a real
// alliance.
const factConfidenceGate = 0.80

// writeFacts resolves each of a row's three numeric fields independently:
// one that failed to parse, or one whose own OCR text a matched member's row
// carries, is queued for review rather than dropped — but that verdict is
// per field, not per row, so a garbled power reading does not cost the row
// its perfectly good level and last_active_hours.
//
// A field's confidence is the minimum of the name match (normalized to
// [0,1]) and that field's own OCR confidence — the same member matched
// confidently can still carry a low-confidence power reading, and the two
// must not be conflated (design doc §4, "Confidence": "a power read at 0.6
// is not a 0.95 fact"). Below factConfidenceGate that confidence is not "a
// 0.95 fact" — CLAUDE.md's invariant #5 makes it not a fact at all, so the
// field is queued for review instead, carrying its own blended confidence so
// a human can see how bad the read was.
//
// The write itself goes through UpsertFact, not InsertFact (task 27). Every
// roster frame in a run shares one observed_at — the capture's own
// started_at, not per-frame wall-clock time, exactly so a replay writes the
// same facts twice rather than new ones each time (see IngestRoster's
// package doc) — so a member whose row genuinely appears in two frames of
// the same capture, or whose facts a previous, crashed attempt at this same
// capture already wrote, computes the identical (member_id, metric,
// period_key, source, observed_at) key both times. InsertFact's plain INSERT
// would reject the second write outright, which is the crash task 27 exists
// to fix; UpsertFact's own doc comment (internal/db/analytics.go) is where
// the append-only-vs-idempotent argument is made in full. Nothing here needs
// to know whether a given write turned out to be new or a no-op — a review
// row is never queued for the repeat, because there is nothing uncertain
// about it that a human could resolve; it is simply not a second fact.
func (run *rosterRun) writeFacts(ctx context.Context, i *Ingester, screenshotID int64, band RowBand, memberID int64, matchNorm float64, fields [3]fieldRead) error {
	for _, f := range fields {
		if f.err != nil {
			if err := run.queueReview(ctx, i, screenshotID, band, f.raw, nil, "unparseable_"+f.label, 0); err != nil {
				return err
			}
			continue
		}
		conf := min(matchNorm, f.conf)
		if conf < factConfidenceGate {
			if err := run.queueReview(ctx, i, screenshotID, band, f.raw, nil, "low_confidence_"+f.label, conf); err != nil {
				return err
			}
			continue
		}
		if _, _, err := i.store.UpsertFact(ctx, db.Fact{
			MemberID: memberID, Metric: f.metric, Value: f.value,
			ObservedAt: run.observedAt, PeriodKey: run.periodKey,
			Source: "ocr:alliance_members", ScreenshotID: screenshotID,
			Confidence: conf,
		}); err != nil {
			return fmt.Errorf("ingest: writing %s fact for member %d: %w", f.metric, memberID, err)
		}
	}
	return nil
}

// queueReview records a row, a whole-frame header, or a single field that
// could not be confidently resolved (or cleared to write), and counts it.
// confidence is 0 when the reason carries no meaningful blended score (an
// unparseable field, an ambiguous name, a full group) — QueueReview stores
// that as SQL NULL, not as a claimed zero-confidence read.
func (run *rosterRun) queueReview(ctx context.Context, i *Ingester, screenshotID int64, band RowBand, rawText string, candidates []roster.Candidate, reason string, confidence float64) error {
	if _, err := i.store.QueueReview(ctx, db.ReviewItem{
		CaptureID: run.captureID, ScreenshotID: screenshotID,
		RowY0: band.Y0, RowY1: band.Y1,
		RawText: rawText, Candidates: candidates, Reason: reason,
		Confidence: confidence,
	}); err != nil {
		return fmt.Errorf("ingest: queueing review (%s): %w", reason, err)
	}
	run.res.Queued++
	return nil
}

// toRosterMembers folds ListMembers and MemberAliases into roster.Member,
// which carries aliases inline for roster.Rank. db.Member and roster.Member
// are deliberately distinct types (the matcher's view, and the database's).
func toRosterMembers(dbMembers []db.Member, aliases map[int64][]string) []roster.Member {
	out := make([]roster.Member, len(dbMembers))
	for idx, m := range dbMembers {
		out[idx] = roster.Member{ID: m.ID, Name: m.Name, Aliases: aliases[m.ID]}
	}
	return out
}

// fieldRect places one field's crop within a row band, as a rect normalized
// to the full frame: xFrac0/1 are fractions of the frame width (columns are
// frame-relative, not row-relative), yFrac0/1 are fractions of the row
// band's own height (rows vary slightly in detected height, so the field
// must scale with the band it was found in).
func fieldRect(band RowBand, img image.Image, xFrac0, xFrac1, yFrac0, yFrac1 float64) transport.Rect {
	h := float64(img.Bounds().Dy())
	rowH := float64(band.Height())
	return transport.Rect{
		X1: xFrac0, X2: xFrac1,
		Y1: (float64(band.Y0) + yFrac0*rowH) / h,
		Y2: (float64(band.Y0) + yFrac1*rowH) / h,
	}
}

// readField preprocesses one region and reads it with the engine. opts
// carries every field's own measured Options (see the vars beside each
// field's ocr.Spec above and in vs.go) — readField sets opts.Region itself
// so callers never have to remember to, which used to mean every field
// silently got Options{} (the full, generally-wrong chain — preprocess.go's
// doc comment) because nothing here overrode it. It is now the caller's job
// to pass its field's measured Options; readField only positions it.
func (i *Ingester) readField(ctx context.Context, img image.Image, rect transport.Rect, spec ocr.Spec, opts vision.Options) (ocr.Result, error) {
	opts.Region = rect
	pre := visionPreprocess(img, opts)
	res, err := i.engine.Read(ctx, pre, spec)
	if err != nil {
		return ocr.Result{}, fmt.Errorf("ingest: ocr read: %w", err)
	}
	return res, nil
}

// readPlan is one way of reading a field: how the pixels are prepared and how
// the engine is configured. Naming the pair matters because the two are only
// meaningful together — options fitted for one page-segmentation mode are not
// evidence about another, which is the same lesson vs.go's crop comment
// records about options fitted through the wrong rectangle.
//
// Not to be confused with fieldRead above, which is one numeric field's
// *outcome*; this is the recipe, that is the result.
type readPlan struct {
	spec ocr.Spec
	opts vision.Options
}

// readFieldWithRetry reads a field, and on an EMPTY read only, reads it a
// second time with a different preparation.
//
// The retry exists for a measured tesseract defect: its layout analysis
// rejects some crops outright, and every layout-analysing mode then returns
// the empty string for text a human reads without effort. Over the 142 row
// bands of capture 6, 15 bands read empty at PSM 7 and every one of them reads
// at PSM 13 — those 15 bands were 12 of the gate's 86 members, none matchable
// at any threshold, because an empty string is not a near miss.
//
// Three properties are deliberate:
//
//   - It retries only on the empty string. A weak read is still evidence and
//     the caller's threshold judges it; an empty one is the specific symptom
//     of layout analysis refusing the crop. Retrying everything is much worse:
//     PSM 13 alone resolves 31/86 members where PSM 7 resolves 62/86.
//   - The retry re-prepares the pixels rather than only changing the mode.
//     Measured over the same bands, retrying with the primary's own options
//     reaches 69/86 and retrying with grayscale+invert at upscale 4 reaches
//     71/86 — the primary's options were fitted for a mode whose layout
//     analysis works, so inheriting them was an assumption, not a result.
//   - It lives here rather than inside the engine, because whether a poor
//     read is safer than no read is a property of the FIELD. A name has a
//     known roster behind it and a bad read simply fails to match; a number
//     has no such guard, and a retry could manufacture a plausible value out
//     of a crop that caught neighbouring content.
func (i *Ingester) readFieldWithRetry(ctx context.Context, img image.Image, rect transport.Rect, primary, retry readPlan) (ocr.Result, error) {
	res, err := i.readField(ctx, img, rect, primary.spec, primary.opts)
	if err != nil || res.Text != "" {
		return res, err
	}
	return i.readField(ctx, img, rect, retry.spec, retry.opts)
}

// visionPreprocess is vision.Preprocess behind a package-level variable
// rather than a direct call, solely so a test can substitute a spy that
// records the Options each readField call actually received. ocr.FakeEngine
// intentionally ignores the pixels it is handed (its own doc comment), so
// without this seam nothing could catch a call site silently reverting to
// the wrong field's Options, or to none at all — exactly the regression
// that shipped capture 1's zero-fact ingest. Production always resolves to
// the real vision.Preprocess; only tests reassign it, and restore it after.
var visionPreprocess = vision.Preprocess

// loadFrame resolves a screenshot to its blob and decodes it. Screenshots
// are always PNG (adb exec-out screencap -p; see CLAUDE.md gotchas).
func (i *Ingester) loadFrame(ctx context.Context, screenshotID int64) (image.Image, error) {
	key, err := i.store.ScreenshotObjectKey(ctx, screenshotID)
	if err != nil {
		return nil, fmt.Errorf("ingest: resolving object key for screenshot %d: %w", screenshotID, err)
	}
	rc, err := i.blobs.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("ingest: fetching blob %s for screenshot %d: %w", key, screenshotID, err)
	}
	defer rc.Close()
	img, err := png.Decode(rc)
	if err != nil {
		return nil, fmt.Errorf("ingest: decoding screenshot %d: %w", screenshotID, err)
	}
	return img, nil
}
