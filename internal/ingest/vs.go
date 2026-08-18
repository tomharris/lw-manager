package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// vsListRegion mirrors tasks.vsListRegion (see internal/tasks/vs_capture.go).
// It is duplicated rather than imported for the same reason
// memberListRegion is duplicated in roster.go: internal/tasks depends on
// internal/runtime, which this device-free package must not pull in, and
// capture_frames.offset_px was measured against exactly this region — ingest
// re-measuring rows against a different one would silently misalign every
// row. If the capture-side region ever moves, this constant must move with
// it. The region already excludes the pinned self row (it sits outside the
// scroll region entirely, per vs_capture.go's own comment), which is the
// belt to roster.Assign's PhaseDuplicate braces: this region keeps the
// pinned copy out of the scroll capture in the first place, and Assign
// catches the case where a duplicate reaches attribution anyway.
var vsListRegion = transport.Rect{X1: 0.03, Y1: 0.185, X2: 0.97, Y2: 0.80}

// vsRowPitch is the ranking row height measured on the handset (see
// internal/tasks/vs_capture.go's constant of the same name and value). It
// differs from the roster's 112px pitch, which is why SegmentRows takes
// pitch as a parameter rather than assuming one.
const vsRowPitch = 128

// Field sub-rects, as fractions of the frame (X) and of one row band's own
// height (Y).
//
// These were recon-estimated from a single frame, then "verified" against
// eight rows by eye, and both readings missed what the first real gate run
// made obvious: every one of the 86 rows failed, 50 of them with an OCR read
// like "at GersonGamer" or "ry} Leroy Jenkins 0914" and 15 with points read
// as "— 17,219,876". Eight rows checked by a reader who already knew what
// they said is not a measurement, and a crop wrong by 40px looks perfectly
// plausible next to a name a human recognizes anyway.
//
// So these are now set from an ink profile over all 142 row bands of the 21
// frames in capture 6 (fixtures/m4gate/expected.yaml's capture), binned at
// 0.02 of frame width and 1/32 of band height:
//
//	rank number      x 0.10..0.16      (then a zero-ink gutter to 0.22)
//	avatar           x 0.22..0.32      full row height, ornate frames included
//	name line        x 0.33..~0.66     y 0.25..0.47
//	alliance line    x 0.34..0.70      y 0.56..0.78
//	points number    x 0.76..0.92      y 0.41..0.61
//
// Three corrections come out of that, and it is worth naming which one each
// symptom needed, because two of them were invisible in the old fixtures:
//
//  1. The name's left edge was 0.26, which is 43px inside the avatar — that
//     is the "at" and "ry}" prefix. Names are left-aligned at 0.333, so 0.33
//     clips no name while leaving at most a sliver of an ornate avatar frame.
//  2. The name's right edge was 0.63, which *truncates* the longest names in
//     this alliance: "MoreBallsThanBrains" runs to 0.66. Nothing occupies
//     0.63..0.72 on the name's own line, so widening costs nothing.
//  3. The points' left edge was 0.65, inside the alliance line's text — the
//     "haos" of "Organized Chaos" is what became a leading dash. 0.74 sits in
//     the middle of a gutter carrying zero ink in all 142 bands.
//
// The points Y range is tightened to bracket the number's own line as well.
// That is defence in depth rather than the fix: with X starting at 0.74 there
// is no alliance text left to catch, but a band that drifts by a few pixels
// should not start reading a neighbouring line.
const (
	vsNameXFrac0, vsNameXFrac1     = 0.33, 0.72
	vsPointsXFrac0, vsPointsXFrac1 = 0.74, 0.97

	vsNameYFrac0, vsNameYFrac1     = 0.05, 0.50
	vsPointsYFrac0, vsPointsYFrac1 = 0.34, 0.68
)

// See roster.go's Spec-value doc comment: these MinConf values document each
// field's OCR difficulty but are advisory, not enforced — factConfidenceGate
// is the floor actually applied, against each field's blended (name-match x
// OCR) confidence.
//
// vsPointsSpec carries no Charset. Task 23's first pass kept "0123456789,"
// here, reasoning that the charset is a superset of what a correct read
// ("101,286,241") is built from -- true, but not sufficient, and the
// fix-round review (finding C1) caught what that reasoning missed: the
// *crop* is not always a correct read. This field's crop sits next to the
// alliance-name line, and on a misaligned or row-straddling band it catches
// fragments of neighboring text and digits together -- e.g. a real crop
// reading "7c 3240 7604" unconstrained (which correctly fails to parse).
// With the whitelist applied to that same crop, tesseract's classifier does
// not just drop the letters, it reclassifies the whole blob into a run of
// digits with none of them left to signal that anything was wrong --
// "3732407604" -- which the old pointsRe's bare-digit-run branch accepted
// outright. The review measured this directly against real pixels: 6 of 11
// bands cut from a committed VS frame produced a parseable number out of
// text that was not a number at all, with values ranging from a plausible
// 5-digit read (44,357 from "44357 OGLE WEF.") to an 12-digit fabrication
// (249,594,593,473 from "24959 n459 3473"). This is the same mechanism
// powerSpec's whitelist had, on the field the M4 gate is actually built
// around (evidence Finding 3) -- see powerSpec's comment in roster.go for
// why "the charset only contains characters a correct read has" was never
// the whole safety argument. Removing it means a row whose crop catches
// neighboring content fails safely to review instead of manufacturing a
// number, matching how power now behaves.
// vsNameLanguages is the tesseract language list for the member-name field, on
// the primary read and the retry alike. It is one constant rather than two
// literals because the two plans were measured together: a list changed on one
// and not the other is not the configuration any number below describes.
//
// The points field deliberately has none. It is digits, and every additional
// language is another way to misread a numeral into something that still
// parses — the same argument as the missing charset above, reached from the
// other side.
//
// Measured over all 142 row bands of capture 6, scored by distinct members
// auto-accepted against the 86 hand-transcribed names:
//
//	eng                    71/86 distinct   (117/142 bands)  <- previous
//	eng+ara                71/86            (117/142)
//	eng+kor                71/86            (116/142)
//	eng+jpn                71/86            (119/142)
//	eng+ell                70/86            (114/142)
//	eng+rus                69/86            (113/142)
//	eng+guj                69/86            (115/142)
//	eng+chi_sim            73/86            (118/142)
//	eng+chi_sim+jpn        73/86            (120/142)
//	eng+kor+chi_sim+jpn    73/86            (120/142)  <- chosen
//	all seven installed    70/86            (116/142)
//
// The whole gain is CJK. Three of the packs this roster's scripts appear to
// call for actively cost members, and rus is the clearest: Cyrillic's capitals
// are drawn like Latin's, so the pack adds hypotheses nothing in the image can
// discriminate between. It turned the auto-accepted "Mar 89" into "Маг 89" and
// dropped "Mc1999" from 76 to 33 on the same read. ara is inert because
// "٣١٢ A l i ٣١٢" is Arabic-Indic *digits* used as decoration, not words, so a
// model carrying word-level priors has nothing to offer it. Install only what
// is listed here; the loaded list is not a free superset.
//
// kor is the one judgement call, since eng+chi_sim+jpn reaches the same 73 on
// the same 120 bands. It trades "OD15" (which drops to a harmless "0015" and
// goes to review) for "한씨아저씨", which without it scores 0 and reads back
// "AKAZA" — what "ΔKΔŽΔ" normalizes to, and that member is auto-accepted. A
// band impersonating another member is worse than a band that matches nobody,
// so kor stays. It does not remove that hazard from the capture, only from
// this row: "2Rule" still loses to "B52RN10" at 100, as it did before any of
// this, and that is a matching problem rather than an OCR one.
//
// This has to be the primary read, not a retry. readFieldWithRetry fires on an
// empty string and there are no empty bands left; the reads these packs fix
// were never empty, only wrong ("Danny 狂" read as "Danny 3t"). Gating a retry
// on a low *match* score instead would put the matcher upstream of OCR.
const vsNameLanguages = "eng+kor+chi_sim+jpn"

var (
	vsNameSpec   = ocr.Spec{MinConf: 0.4, Languages: vsNameLanguages}
	vsPointsSpec = ocr.Spec{MinConf: 0.6}
)

// vsNameOptions and vsPointsOptions: grayscale + upscale(3), measured against
// eight real rows of docs/superpowers/specs/evidence/m4-scrolloffset-2026-08-13's
// committed VS weekly-ranking frames (01/02-vs-weekly-frame-*.png). Both
// fields read 8/8 exactly — vsPointsOptions reproduces the evidence README's
// own finding 3 ("rank -> 6, points -> 101,286,241") across the other seven
// rows too, unlike roster.go's powerOptions: VS points are plain digits and
// commas with no decimal point to lose, which is the field difference that
// explains the gap between the two numeric fields' results.
// Re-measured after the crop corrections above, because options fitted to a
// crop that turned out to include 43px of avatar are not evidence about the
// crop that does not. The sweep ran all eight skip-flag shapes at upscale
// 2/3/4 over all 142 row bands of capture 6, scoring each by how many
// distinct members its reads auto-accepted against the 86 hand-transcribed
// names — the quantity the gate actually turns on, rather than a substring
// count that has to be aligned to rows by hand.
//
//	gray          x2   62/86 distinct   (103/142 bands, 15 empty)  <- chosen
//	gray          x3   61/86            ( 99/142,       15 empty)  <- previous
//	gray+inv      x2   62/86            (103/142,       15 empty)
//	gray+thr      x2   54/86            ( 85/142,       21 empty)
//	full          x2   22/86            ( 32/142,       56 empty)
//
// The expectation going in was that thresholding would now help — it had been
// skipped because it destroyed a crop dominated by a colourful avatar, and
// that reason was gone. The measurement says otherwise: thresholding costs 8
// members and doubles the empty reads even on the clean crop, and the full
// chain is catastrophic. Recorded because it is a plausible thing to try
// again; it has been tried, on real rows, and it is worse.
//
// Only the upscale factor moved, x3 to x2, and it is worth one member rather
// than a breakthrough. What the sweep really establishes is a ceiling: at the
// best setting 24 of 86 members still never auto-accept, and 15 bands read
// back empty.
//
// This comment used to attribute those 15 empty bands to the names rendered in
// Korean, Arabic and CJK. That was wrong twice over, and both corrections are
// load-bearing for anyone re-fitting these constants. Per member, the empty
// bands were nearly all plain ASCII and the non-Latin names were never empty —
// see the language-pack table above, where the packs that would have fixed a
// script problem cost members instead. The empties were tesseract's layout
// analysis giving up, which is what the PSM 13 retry below now recovers: at
// this setting there are no empty bands left at all.
var (
	vsNameOptions   = vision.Options{SkipEqualize: true, SkipThreshold: true, SkipInvert: true, UpscaleFactor: 2}
	vsPointsOptions = vision.Options{SkipEqualize: true, SkipThreshold: true, SkipInvert: true, UpscaleFactor: 3}
)

// vsName is how a ranking row's name is read, and vsNameRetry is how it is
// read again when the first attempt returns nothing at all. See
// readFieldWithRetry for why the retry exists and why it re-prepares the
// pixels instead of only changing the mode.
//
// The retry's options are measured, not inherited. Sweeping all eight
// preprocess shapes at upscale 2/3/4 for the retry alone, with the primary
// held fixed, over all 142 row bands of capture 6:
//
//	gray+inv  x4   71/86 distinct   (117/142 bands)  <- chosen
//	gray      x4   70/86            (116/142)
//	gray      x2   69/86            (114/142)        <- what inheriting gives
//	gray+eq   x3   66/86            (110/142)
//	full      x2   66/86            (110/142)
//
// Upscale 4 is most of it — both x4 shapes clear 70 — and inverting is worth
// one further member. The margin over inheriting is two members on fifteen
// bands, so it is a real result on this capture rather than a large one; if a
// later capture disagrees, re-run `make probe-m4 PROBE_ARGS=-probe.fbsweep`
// and believe the newer measurement.
//
// The points field's retry is vsPointsRetry, below. The asymmetry between the
// two fields used to be absolute -- a name that reads badly fails to match a
// known roster and goes to review, while a number has no such guard, and a
// raw-line retry on a crop that caught neighbouring content could manufacture
// a plausible value, which is exactly the failure the vsPointsSpec charset
// comment above describes. It is now conditional rather than absolute: the
// points bounds in points.go answer that objection for a retried read the
// same way a roster answers it for a retried name, provided a retried value
// is never allowed to seed the bound it is then judged against -- see
// vsRow.PointsFromRetry.
var (
	vsName      = readPlan{spec: vsNameSpec, opts: vsNameOptions}
	vsNameRetry = readPlan{
		spec: ocr.Spec{MinConf: vsNameSpec.MinConf, PSM: ocr.PSMRawLine, Languages: vsNameLanguages},
		opts: vision.Options{SkipEqualize: true, SkipThreshold: true, UpscaleFactor: 4},
	}
)

// vsNameModes are the segmentation modes every name crop is read at. Each
// produces its own read; the per-member best score across all of them is what
// the assignment sees.
//
// PSM 13 alone is much worse than PSM 7 alone -- 55/86 members against 73/86
// -- but their miss sets are DISJOINT in four places on capture 6, so the
// union beats either. Worth +6 at the per-row baseline and +1 once closed-set
// assignment is in, which makes this insurance rather than accuracy: it earns
// its keep on a capture where the assignment has less structure to work with,
// which is exactly the case a single fixture cannot measure.
//
// This is not the thing CLAUDE.md warns against when it says gating a retry on
// a low match score "would put the matcher upstream of OCR". Both reads happen
// unconditionally, so nothing about the roster decides what OCR is asked to
// do; taking the better of two independent readings of the same pixels is the
// move the probes already make across overlapping frames.
//
// Cost is one extra tesseract invocation per row, roughly 27s to 53s over 86
// rows, in a batch that runs once a day.
var vsNameModes = []readPlan{
	vsName,
	{spec: ocr.Spec{MinConf: vsNameSpec.MinConf, PSM: ocr.PSMRawLine, Languages: vsNameLanguages}, opts: vsNameOptions},
}

// vsPointsRetry reads a points crop the primary PSM returned nothing for.
//
// The asymmetry with the name field was deliberate and is now conditional
// rather than absolute. A name has a known roster behind it, so a bad read
// simply fails to match; a number has no such guard, and a raw-line retry
// could manufacture a plausible value from a crop that caught neighbouring
// content. That objection is answered by the bounds in points.go and not
// otherwise: a retried value is written only when it parses AND lands inside
// the window its neighbours define, which a fabricated number has no reason
// to do -- and never seeds a window itself, regardless of confidence, so a
// fabrication can never define the range its neighbours are judged against
// (see vsRow.PointsFromRetry and the seeding loop in IngestVS).
//
// It keeps vsPointsOptions rather than borrowing vsNameRetry's grayscale-plus-
// invert at upscale 4, and this time inheriting is a measured result rather
// than an assumption. `make probe-points PROBE_ARGS='-points.fbsweep'` sweeps
// the retry's preprocessing over the same eight-shape grid while the primary
// stays fixed at the shipped setting, over all 142 row bands of capture 6:
//
//	gray      x2/x3/x4   83/86 rows exact   (135/142 bands, 0 empty)  <- chosen (== vsPointsOptions)
//	gray+inv  x2/x3/x4   83/86              (135/142,       0 empty)  <- tied
//	full      x2/x3/x4   82/86              (133/142,       0 empty)
//	gray+eq   x2/x3/x4   82/86              (133/142,       0 empty)
//	gray+thr  x2/x3/x4   82/86              (133/142,       0 empty)
//	(the remaining two-flag combinations tie with the single-flag ones above)
//
// Two things fall out of this, and neither is what the name-side sweep found:
//
//   - Upscale does not move the result at all -- x2, x3 and x4 score
//     identically within every shape. That is not the broken-instrument
//     uniformity CLAUDE.md warns about (which was identical scores ACROSS
//     shapes that differ enormously elsewhere); shapes here still separate
//     82 from 83. It is explained by how few bands the retry ever touches --
//     measured, not inferred from the before/after aggregate above: `make
//     probe-points` reports a Retried count alongside the rest of the row,
//     and it reads 4 of 142 bands on this capture. That is far less surface
//     for an upscale factor to move than the name field's retry, which was
//     fitted against fifteen empty bands.
//   - Grayscale-alone beats grayscale-plus-invert's name-side win by exactly
//     tying it rather than losing to it -- inverting costs nothing here but
//     also buys nothing, unlike the name retry where it was worth one
//     member. vsPointsOptions already IS "gray x3" in this grid, so the
//     inheritance this comment used to only assume is now a checked result:
//     the primary's options happen to be optimal for the retry too, on this
//     capture.
//
// The three points failures this unblocks on the M4 gate's capture 6 read
// empty at the primary and now read close but not exact at the retry --
// "12,090,000 —", "¢,609,299", "e,2¢8,001" for ranks 20, 77 and 79 -- each
// containing the right digits corrupted by a stray symbol raw-line mode
// hallucinates from crop noise. pointsRe correctly refuses all three at this
// point in the pipeline, so they stay queued as unparseable_points rather
// than writing a near-miss value directly out of this retry -- loosening
// pointsRe to admit these would be the same mistake as lowering
// roster.AutoAccept.
//
// Two of the three are recovered downstream instead, by mechanisms that are
// not part of this retry and do not loosen this regex at all.
// trimPointsArtifact (points.go) strips rank 20's trailing " —" before
// ParsePoints ever runs, because every digit in that read was already right.
// repairPoints (points.go) solves for rank 77's single symbol-for-digit
// character subject to the ordering bounds, because the bounds admit exactly
// one digit there. Rank 79 has two damaged positions -- "e,2¢8,001" -- which
// repairPoints refuses by construction: two unknowns is a guess with extra
// steps, whatever the bounds say, and it stays queued as unparseable_points.
// See the task 10 report for the exact before/after gate line.
var vsPointsRetry = readPlan{
	spec: ocr.Spec{MinConf: vsPointsSpec.MinConf, PSM: ocr.PSMRawLine},
	opts: vsPointsOptions,
}

// zeroInferenceConfidence is the confidence an inferred (not read) zero
// carries. The weekly ranking lists only members with a nonzero score, so a
// member absent from a *complete* capture can be inferred to have scored
// zero — but that inference is weaker evidence than a number actually read
// off the screen: an OCR fact points at a specific crop of a specific
// screenshot, while an inferred fact can only point at the capture as a
// whole and the completeness proof it depends on. A leaderboard consumer
// must be able to tell "we saw a zero" from "we saw nothing and concluded
// zero" even though both clear the write threshold, which is why this sits
// below every OCR-derived confidence this package writes rather than at (or
// above) 1.0.
const zeroInferenceConfidence = 0.90

// residualMatchConfidence is the name-match confidence a phase-2 assignment
// carries.
//
// It exists because score/100 is the wrong confidence model for an
// assignment, and using it would silently erase the whole gain: a row
// resolved at string-score 60 blends to 0.60, falls under
// factConfidenceGate (0.80), and is queued for review despite having been
// resolved. Lowering that gate is not the fix -- it protects every other row.
//
// The claim a phase-2 assignment makes is not "these two strings are 60%
// similar". It is "this member is the unambiguous winner among the
// unclaimed, by a margin of at least ResidualMargin, in a closed set where
// every confident row is already pinned." That claim's strength does not vary
// with the string score, so neither does this number.
//
// 0.85 sits above factConfidenceGate and visibly below a clean match, so the
// distinction survives into the fact and a human triaging later can see how a
// row was resolved. UpsertFact only overwrites on strictly higher confidence,
// so a later clean read of the same member supersedes a residual match by
// itself -- which is the correct direction.
//
// Fitted, like confusableCost. Re-measure with `make probe-assign`; do not
// re-reason.
const residualMatchConfidence = 0.85

// There used to be a pointsOrderConfidenceFloor here: a 0.40 OCR-confidence
// gate a row's own points read had to clear, in addition to a CLOSED window
// on both sides, before promotion to factConfidenceGate. It was fitted
// against two POSITIVE examples only -- correct reads at 0.52 (rank 38) and
// 0.70 (rank 83) -- and nobody had checked what it excluded.
//
// Measured directly against capture 6
// (.superpowers/sdd/2026-08-17-m4-closed-set-matching/investigation-confidence-floor.md):
// the population it actually gated -- a points read that parses and sits in
// a window closed on both sides -- has exactly ONE row below 0.40 on this
// capture, rank 10 (16,958,260 at confidence 0.0853), and it is CORRECT.
// Zero rows in that population, above or below 0.40, are wrong. The two
// reads that genuinely ARE wrong on this capture (ranks 77, 79) never reach
// this population at all -- they fail to PARSE, so they are refused two
// steps earlier, before confidence is ever consulted (see repairPoints and
// the late retry below). So the floor was screening nothing here, and its
// measured cost was one correct, structurally-corroborated row sent to
// review for no offsetting protection. Removed.
//
// n=1 in the population this measured is thin, and that is the part worth
// not forgetting: if a later capture ever shows a genuinely WRONG value that
// parses and lands inside a closed window, at any confidence, the floor's
// argument comes back -- and it must be refit against the set it EXCLUDES
// this time, not (as happened the first time) the set it admits.

// VSResult summarizes one IngestVS run.
//
// Unidentified counts rows whose name matched no member confidently enough to
// attribute. It is reported rather than merely used because it is what
// explains a run that wrote no zeroes: "nobody was absent" and "we declined to
// guess who was absent" are different outcomes that both print Zeroed=0, and
// an operator who cannot tell them apart cannot tell a clean capture from one
// that needs its review queue cleared.
type VSResult struct {
	Matched, Queued, Zeroed, Unidentified int
	// Duplicates counts rows dropped because the member they read was
	// already assigned to another row -- the pinned self row, structurally.
	// Reported rather than silent because "the capture contained a duplicate"
	// and "the capture contained a row nobody could attribute" are different
	// outcomes, and only one of them is a problem.
	Duplicates int
	Status     string
}

// vsRun carries the state one IngestVS call accumulates across frames.
type vsRun struct {
	captureID  int64
	observedAt time.Time
	periodKey  string
	res        VSResult
}

// IngestVS turns one VS-ranking capture's frames into vs_points facts.
//
// Unlike IngestRoster, completeness is never recomputed from what got
// parsed: captures.status was set at capture time by whichever route proved
// (or failed to prove) it reached the bottom of the list — see
// db.Pool.RecordCapture's own doc comment — and that is the only evidence
// this function trusts for the zero-inference rule below. A capture that
// merely parsed every row it happened to segment is not the same claim, and
// inferring completeness from row counts here would defeat the whole point
// of storing status at capture time.
func (i *Ingester) IngestVS(ctx context.Context, captureID int64, periodKey string) (VSResult, error) {
	capture, err := i.store.Capture(ctx, captureID)
	if err != nil {
		return VSResult{}, fmt.Errorf("ingest: loading capture %d: %w", captureID, err)
	}
	frames, err := i.store.CaptureFrames(ctx, captureID)
	if err != nil {
		return VSResult{}, fmt.Errorf("ingest: loading frames for capture %d: %w", captureID, err)
	}

	// See roster.go's IngestRoster for why this wrap names `control alliance
	// set` rather than passing CurrentAllianceID's bare ErrNotFound through:
	// the same fresh-deployment dead end, reached by the other ingest route.
	allianceID, err := i.store.CurrentAllianceID(ctx)
	if err != nil {
		return VSResult{}, fmt.Errorf("ingest: resolving current alliance (run `control alliance set --tag <tag> --name <name>` first): %w", err)
	}
	dbMembers, err := i.store.ListMembers(ctx, allianceID)
	if err != nil {
		return VSResult{}, fmt.Errorf("ingest: listing members for alliance %d: %w", allianceID, err)
	}
	aliases, err := i.store.MemberAliases(ctx, allianceID)
	if err != nil {
		return VSResult{}, fmt.Errorf("ingest: listing aliases for alliance %d: %w", allianceID, err)
	}
	members := toRosterMembers(dbMembers, aliases)

	// The safety half of confusable-aware scoring, checked against the roster
	// this run will actually match against. Two members scoring at or above
	// AutoAccept are a pair the matcher cannot tell apart, which no threshold
	// fixes and only an alias can -- and attributing one member's score to the
	// other is the single failure here a review queue cannot undo.
	//
	// Over the WHOLE member list, not the ranked rows. `make probe-m4`
	// measured it over the 86 transcribed names, which is the set least
	// likely to contain the problem: the ranking lists scorers only, so the
	// near-neighbour that breaks a match is disproportionately a member who
	// did not score and was therefore invisible to the check.
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	if closest, a, b := roster.ClosestPairScore(names); closest >= roster.AutoAccept {
		// ClosestPairScore reports only the single highest-scoring pair, so
		// this is the closest pair, not necessarily the only one at or above
		// AutoAccept -- a third or fourth near-indistinguishable pair could
		// exist below it and this warning would not say so.
		slog.WarnContext(ctx, "ingest: two roster members are indistinguishable to the matcher (closest pair; others may exist below it)",
			"capture_id", captureID, "score", closest, "auto_accept", roster.AutoAccept,
			"member_a", a, "member_b", b)
	}

	// observedAt is the capture's own started_at, not wall-clock now — see
	// IngestRoster's doc comment for why: a replayed ingest run must write
	// the same facts it would have written on capture day.
	observedAt := capture.StartedAt
	run := &vsRun{
		captureID:  captureID,
		observedAt: observedAt,
		periodKey:  periodKey,
		res:        VSResult{Status: capture.Status},
	}

	rows, err := i.readVSRows(ctx, frames, members)
	if err != nil {
		return VSResult{}, err
	}
	totalParsed := len(rows)
	lastFrameShotID := int64(0)
	if len(frames) > 0 {
		lastFrameShotID = frames[len(frames)-1].ScreenshotID
	}

	assignments := roster.Assign(scoreMatrix(rows), roster.DefaultResidual)

	// assignedMember's keys are MEMBER indices -- roster.Assign's column
	// space, the same indexing as `members` and every vsRow.Scores, not row
	// position. It exists only for the zero-inference loop below: a member
	// who already has a row, confident or residual, is not absent from the
	// capture and must not be zeroed. Duplicate detection no longer needs it
	// -- roster.Assign now reports a duplicate row directly via
	// PhaseDuplicate, see below.
	assignedMember := make(map[int]bool, len(assignments))
	for _, a := range assignments {
		if a.Member >= 0 {
			assignedMember[a.Member] = true
		}
	}

	// Points resolve against the ranking's own order, which needs every row's
	// value in hand -- so parse first, bound second, write third. Rows are in
	// rank order because they are in screen order.
	values := make([]int64, len(rows))
	known := make([]bool, len(rows))
	for n, row := range rows {
		// A row without a confident NAME match is excluded from seeding even
		// when its points read is itself confident and would otherwise
		// tighten its neighbours' windows for free. That is deliberate
		// conservatism, not an oversight: a row that reached here without a
		// name match includes a spurious segmentation band (no real row at
		// all), and letting an ungrounded band seed a bound risks narrowing
		// real neighbours' windows off of content that was never a ranking
		// row to begin with. The cost is a missed tightening opportunity on
		// an already-review-bound row; the alternative risks corrupting
		// rows that would otherwise resolve cleanly.
		if assignments[n].Member < 0 {
			continue
		}
		// trimPointsArtifact runs here too, not just in attributeRow below:
		// it constructs nothing, so there is no reason to withhold a row like
		// rank 20's from seeding just because a trailing artifact would
		// otherwise have failed the regex. repairPoints, by contrast, is
		// never called in this loop -- a repaired value must never seed a
		// bound (see repairPoints' own comment), and the simplest way to
		// guarantee that is for this loop to never call it at all.
		v, err := ParsePoints(trimPointsArtifact(row.PointsText))
		if err != nil {
			continue
		}
		// Only a confident read seeds a bound. A weak read is what the bounds
		// exist to adjudicate, and letting it define the window it is judged
		// against would make the check circular.
		//
		// A retried read is excluded from seeding regardless of what
		// confidence it reports, which is a stronger and separate condition
		// from the confidence check above, not a special case of it. The
		// confidence gate assumes a low number means a weak read and a high
		// one means a trustworthy one -- true for the primary read, but a
		// PSM-13 raw-line retry only runs because the primary came back
		// empty, and it can report high confidence for a value manufactured
		// from a crop that caught neighbouring content (see vsPointsRetry's
		// comment). Letting a retried read seed a window would let a
		// fabrication define the range its neighbours are judged against --
		// the exact failure the bounds exist to prevent, reintroduced one
		// level up. A retried value is still eligible to be WRITTEN, subject
		// to the bounds its real neighbours define and the promotion rules
		// below; it just may never define anyone else's window.
		if row.PointsConf >= factConfidenceGate && !row.PointsFromRetry {
			values[n], known[n] = v, true
		}
	}
	// A seed that is greater than the nearest better-ranked seed already
	// accepted cannot be a genuine ranking row -- see monotonicKnown's own
	// comment for why a value like that (most notably the pinned self row,
	// which Task 3's assignment keeps out of this array by Member < 0, but
	// which does not by itself rule out every other kind of out-of-order
	// value) must be dropped rather than allowed to seed a window.
	known = monotonicKnown(ctx, values, known)
	bounds := pointsBounds(values, known)

	for n, row := range rows {
		a := assignments[n]
		dup := a.Phase == roster.PhaseDuplicate
		matched, err := run.attributeRow(ctx, i, row, a, dup, bounds[n], members)
		if err != nil {
			return VSResult{}, err
		}
		switch {
		case dup:
			run.res.Duplicates++
		case matched:
			// counted inside attributeRow via run.res.Matched
		default:
			run.res.Unidentified++
		}
	}

	// Absence means zero, but only on a complete capture whose every row was
	// attributed. The weekly ranking lists only members with a nonzero score
	// (recon measured 94 ranked rows against 96 alliance members), so a member
	// missing from a complete capture genuinely scored nothing.
	//
	// Two conditions, for two different ways that inference goes wrong:
	//
	// On a partial capture, absence and truncation are indistinguishable, so
	// a capture wrongly treated as complete would silently zero real scores
	// for exactly the members hardest to see.
	//
	// The second condition is subtler and was found by the resolve-then-
	// reingest test below. "Missing from the capture" and "present but not
	// confidently matched" are not the same claim, and this loop can only see
	// the first: an unidentified row belongs to *some* member, and that
	// member is not in scored, so they were being zeroed on the strength of a
	// row we were holding in the review queue at that very moment. That is a
	// confident number (zeroInferenceConfidence, 0.90) on a leaderboard for a
	// read that failed, which invariant #5 exists to forbid, and it also
	// broke the correction path: UpsertFact only overwrites on a strictly
	// higher confidence, so the stale zero outranked the real read the
	// re-ingest produced and the resolved review silently changed nothing.
	//
	// So a capture holding rows it could not attribute has not proved anyone
	// absent, and infers nothing. The zeroes are not lost, only deferred:
	// clear the review queue and re-ingest, and this same capture writes them
	// once every row is accounted for.
	if capture.Status == "complete" && run.res.Unidentified == 0 {
		for n, m := range members {
			if assignedMember[n] {
				continue
			}
			if _, _, err := i.store.UpsertFact(ctx, db.Fact{
				MemberID: m.ID, Metric: "vs_points", Value: 0,
				ObservedAt: observedAt, PeriodKey: periodKey,
				Source: "ocr:vs_ranking", ScreenshotID: lastFrameShotID,
				Confidence: zeroInferenceConfidence,
			}); err != nil {
				return VSResult{}, fmt.Errorf("ingest: writing an inferred zero for member %d: %w", m.ID, err)
			}
			run.res.Zeroed++
		}
	}

	if err := i.store.FinishCapture(ctx, captureID, capture.Status, totalParsed, ""); err != nil {
		return VSResult{}, fmt.Errorf("ingest: finishing capture %d: %w", captureID, err)
	}
	return run.res, nil
}

// vsRow is one deduped ranking row: where it came from and what both fields
// read, held until every row has been read.
//
// Assignment cannot be streamed -- no row may be attributed until every row's
// scores are known -- so the frame walk stops writing anything and accumulates
// these instead. That also strengthens invariant #2 rather than straining it:
// a crash midway through the walk now leaves nothing written, where it used to
// leave a partial set of facts.
type vsRow struct {
	ScreenshotID int64
	Band         RowBand
	NameText     string
	PointsText   string
	PointsConf   float64
	// PointsFromRetry records whether PointsText/PointsConf came from
	// vsPointsRetry rather than the primary read -- a controller-level
	// requirement, not something the retry's own gating implies.
	// readFieldWithRetry already restricts WHEN the retry runs (an empty
	// primary read, and nothing else); this is about what a retried value is
	// allowed to DO once it exists. A PSM-13 raw-line retry can report a high
	// OCR confidence while being the least trustworthy read in the pipeline --
	// it only ran because the trustworthy mode returned nothing -- so if it
	// were allowed to seed a neighbour's bound the way any other confident
	// read does (see the seeding loop in IngestVS), a fabrication could define
	// the window its neighbours are then judged against: exactly the failure
	// the bounds exist to prevent, reintroduced one level up. A retried value
	// stays eligible to be WRITTEN under the ordinary bounds and promotion
	// rules; it just may never be the thing that defines someone else's
	// window.
	PointsFromRetry bool
	// Scores is this row's name score against every member, indexed the same
	// as the members slice IngestVS built. It comes from roster.Rank, so
	// member aliases are already folded in and the assignment never has to
	// know they exist.
	Scores []int
}

// vsRowCursor replays the geometric dedupe across a capture's frames.
//
// It is a type rather than four local variables so the probes can share it.
// An instrument that reimplements the dedupe drifts from what IngestVS does,
// and then reports a row set production never sees -- which is the failure
// mode CLAUDE.md records for the probe whose retry was happening inside the
// engine before the probe ever saw it.
type vsRowCursor struct {
	contentY int
	lastRowY int
	havePrev bool
}

func newVSRowCursor() *vsRowCursor { return &vsRowCursor{lastRowY: -1} }

// nextFrame advances to a new frame, accumulating its measured scroll offset.
func (c *vsRowCursor) nextFrame(offsetPx int) {
	if c.havePrev {
		c.contentY += offsetPx
	}
	c.havePrev = true
}

// accept reports whether a band is a new row rather than a geometric
// duplicate of the one before it, and advances the cursor when it is.
func (c *vsRowCursor) accept(band RowBand, regionTop int) bool {
	rowY := c.contentY + (band.Y0 - regionTop)
	if c.lastRowY >= 0 && rowY <= c.lastRowY+vsRowPitch/2 {
		return false
	}
	c.lastRowY = rowY
	return true
}

// readVSRows is pass 1: walk the frames, drop geometric duplicates, and read
// both fields of every surviving row. It writes nothing.
//
// The field order -- name read at every vsNameModes entry in order, then
// points, per band -- is load-bearing for the tests: ocr.FakeEngine returns
// queued results in call order and every VS fixture builds that queue
// name-name-then-points per row (one name result per vsNameModes entry, plus
// one more for either read's own retry if it comes back empty).
func (i *Ingester) readVSRows(ctx context.Context, frames []db.CaptureFrame, members []roster.Member) ([]vsRow, error) {
	idxByID := make(map[int64]int, len(members))
	for n, m := range members {
		idxByID[m.ID] = n
	}

	var rows []vsRow
	cursor := newVSRowCursor()

	for _, frame := range frames {
		img, err := i.loadFrame(ctx, frame.ScreenshotID)
		if err != nil {
			return nil, fmt.Errorf("ingest: loading screenshot %d: %w", frame.ScreenshotID, err)
		}
		cursor.nextFrame(frame.OffsetPx)

		bands, err := SegmentRows(img, vsListRegion, vsRowPitch)
		if err != nil {
			return nil, fmt.Errorf("ingest: segmenting screenshot %d: %w", frame.ScreenshotID, err)
		}
		regionTop := int(vsListRegion.Y1 * float64(img.Bounds().Dy()))

		for _, band := range bands {
			if !cursor.accept(band, regionTop) {
				continue // geometric duplicate; OCR never runs on it
			}

			scores := make([]int, len(members))
			nameText := ""
			bestTop := -1
			for _, mode := range vsNameModes {
				nameRes, _, err := i.readFieldWithRetry(ctx, img,
					fieldRect(band, img, vsNameXFrac0, vsNameXFrac1, vsNameYFrac0, vsNameYFrac1),
					mode, vsNameRetry)
				if err != nil {
					return nil, err
				}
				if nameRes.Text == "" {
					continue
				}
				top := 0
				for _, c := range roster.Rank(nameRes.Text, members) {
					if j := idxByID[c.MemberID]; c.Score > scores[j] {
						scores[j] = c.Score
					}
					if c.Score > top {
						top = c.Score
					}
				}
				// NameText is only ever shown to a human in the review queue,
				// so it carries whichever read got closest to a real member --
				// the one worth looking at when deciding what the row says.
				if top > bestTop {
					bestTop, nameText = top, nameRes.Text
				}
			}

			pointsRes, pointsRetried, err := i.readFieldWithRetry(ctx, img,
				fieldRect(band, img, vsPointsXFrac0, vsPointsXFrac1, vsPointsYFrac0, vsPointsYFrac1),
				readPlan{spec: vsPointsSpec, opts: vsPointsOptions}, vsPointsRetry)
			if err != nil {
				return nil, err
			}

			rows = append(rows, vsRow{
				ScreenshotID:    frame.ScreenshotID,
				Band:            band,
				NameText:        nameText,
				PointsText:      pointsRes.Text,
				PointsConf:      pointsRes.Confidence,
				PointsFromRetry: pointsRetried,
				Scores:          scores,
			})
		}
	}
	return rows, nil
}

// scoreMatrix projects the rows' score vectors into the shape roster.Assign
// takes.
func scoreMatrix(rows []vsRow) [][]int {
	out := make([][]int, len(rows))
	for i, r := range rows {
		out[i] = r.Scores
	}
	return out
}

// candidatesFor rebuilds the ranked candidate list from a row's stored score
// vector, so a review row can still record what the matcher was choosing
// between without pass 1 carrying a second copy of it.
func candidatesFor(row vsRow, members []roster.Member) []roster.Candidate {
	out := make([]roster.Candidate, 0, len(members))
	for j, m := range members {
		out = append(out, roster.Candidate{MemberID: m.ID, Name: m.Name, Score: row.Scores[j]})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}

// attributeRow routes one already-read row to a fact, a duplicate no-op, or
// the review queue. It never creates a member: the roster route is the only
// writer of that table, so a row matching nothing goes to review, full stop.
//
// It no longer decides WHO the row is -- roster.Assign did that across the
// whole capture, using the constraint that a member appears at most once.
// What is left here is what to do about it.
func (run *vsRun) attributeRow(ctx context.Context, i *Ingester, row vsRow, a roster.Assignment, dup bool, bound pointsBound, members []roster.Member) (bool, error) {
	if dup {
		// Same member, a different screen position: the pinned self row.
		// Counted by the caller, deliberately not queued.
		return false, nil
	}
	if a.Member < 0 {
		candidates := candidatesFor(row, members)
		reason := "no_confident_match"
		if len(candidates) > 0 && candidates[0].Score >= roster.ReviewFloor {
			reason = "ambiguous_name_match"
		}
		return false, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.NameText, candidates, reason, 0)
	}

	memberID := members[a.Member].ID
	run.res.Matched++

	// A phase-1 match is a string that scored >= AutoAccept and is weighted
	// as one. A phase-2 match is an elimination argument whose strength does
	// not vary with the string score -- see residualMatchConfidence.
	matchNorm := float64(a.Score) / 100.0
	if a.Phase == roster.PhaseResidual {
		matchNorm = residualMatchConfidence
	}

	// Order matters and is not arbitrary: trim first, then parse, then --
	// only if that still fails -- attempt repair.
	//
	// Trimming has to come before parsing because it is what makes rank 20's
	// otherwise-correct read parse at all; running it after a failed parse
	// would work too, but running it first means the failed-parse path below
	// only ever sees a read that is genuinely unparseable rather than one
	// that merely had a trailing artifact, which is exactly what
	// repairPoints below needs to be true of its input.
	//
	// repairPoints runs last, and only as a fallback, because it is the only
	// step in this pipeline that CONSTRUCTS a value rather than validating
	// one. Trying it before the plain parse would mean fencing every read
	// through repairPoints' stricter, guess-refusing logic even when the
	// trimmed text was already well-formed -- backwards for a check that
	// exists specifically to be a last resort. It runs on the TRIMMED text,
	// not the raw one, so a trailing artifact on an otherwise-repairable read
	// cannot masquerade as a second damaged position and cause a correct
	// repair to be refused for the wrong reason.
	trimmed := trimPointsArtifact(row.PointsText)
	points, perr := ParsePoints(trimmed)
	if perr != nil {
		if v, ok := repairPoints(trimmed, bound); ok {
			// A repaired value never seeds another row's bound -- not
			// because of a check here, but because it structurally cannot:
			// every row's bound was already computed, from every row's
			// UNREPAIRED read, before this per-row loop began (see the
			// seeding loop above). It still carries the read's own
			// confidence and still points at the same crop of the same
			// screenshot below, exactly like any other read; it earns no
			// confidence boost for having been repaired.
			points, perr = v, nil
		}
	}
	if perr != nil {
		return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "unparseable_points", 0)
	}
	if !withinBounds(points, bound) {
		// A value outside the window its neighbours define is the signature
		// of a crop that caught neighbouring content -- the failure
		// vsPointsSpec's charset comment describes, caught structurally.
		return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "points_out_of_order", 0)
	}
	// Everything past here is in order, but "in order" alone does not mean
	// "corroborated": the windows are open at the ends -- out[0].Hi is
	// always MaxInt64, out[len-1].Lo is always 0 -- and when NOTHING seeds a
	// bound at all, every window is [0, MaxInt64], so withinBounds is
	// unconditionally true and this check degrades to "did the regex match",
	// which is the guard the old low_confidence_points branch already
	// applied on its own. Promoting on that alone would invert the failure
	// mode: a capture where OCR broadly degrades used to queue everything,
	// and would instead write everything at factConfidenceGate with an empty
	// review queue -- worse OCR producing fewer queued rows. So promotion
	// requires the window to be CLOSED on both sides -- an actual pair of
	// neighbours bracketing the value, not an absent constraint that lets
	// anything through. It is raised to exactly factConfidenceGate and no
	// further: invariant #5 is about what a number claims about itself, and
	// being in order does not make a 0.09 read a 0.95 one -- it only makes it
	// worth writing once the ordering has actually corroborated it.
	//
	// There is deliberately no additional floor on the read's own OCR
	// confidence here (see the removed pointsOrderConfidenceFloor comment
	// above vsRun.attributeRow's neighbourhood -- the const used to live
	// where this comment now does). Measured against capture 6, the row this
	// mattered for is rank 10 at confidence 0.0853, correct, and the only row
	// a floor would have excluded from this exact population; nothing wrong
	// has ever been observed riding a closed window through here at any
	// confidence. If that changes on a later capture, refit the floor against
	// what it excludes, not what it admits -- that is the mistake its first
	// fitting made.
	//
	// This means a RETRIED row's own PointsConf can still promote it here,
	// even though that identical confidence is exactly what the seeding loop
	// above refuses to trust at all (see vsRow.PointsFromRetry). That is not
	// a contradiction -- the two checks ask different questions of the same
	// number. Seeding lets a value constrain OTHER rows with nothing external
	// checking it: a retried value seeding a window is one unverified read
	// defining the range its neighbours are then judged against, which is
	// exactly the fabrication risk the bounds exist to prevent, reintroduced
	// one level up. Promotion runs the other way: it requires two REAL,
	// independent neighbours' seeds to already bracket the value on both
	// sides before its own confidence is even consulted, so the structural
	// evidence points AT the value rather than OUT from it toward a row that
	// has not earned any corroboration of its own. A fabricated retry read
	// has no reason to land inside a window it played no part in drawing, so
	// closure alone is doing, in this context, the work a confidence floor
	// would otherwise be asked to do for a row with no such bracketing.
	conf := min(matchNorm, row.PointsConf)
	corroborated := bound.Lo > 0 && bound.Hi < math.MaxInt64
	if corroborated && conf < factConfidenceGate {
		conf = factConfidenceGate
	}
	if conf < factConfidenceGate {
		return true, run.queueReview(ctx, i, row.ScreenshotID, row.Band, row.PointsText, nil, "low_confidence_points", conf)
	}
	// UpsertFact, not InsertFact, for the reason IngestRoster's writeFacts
	// documents at length: observed_at is pinned to the capture's own
	// started_at, so re-running ingest over this same capture recomputes the
	// identical (member_id, metric, period_key, source, observed_at) key and
	// a plain INSERT rejects it -- which is not hypothetical, since resolving
	// a review tells the operator to ingest the capture again.
	if _, _, err := i.store.UpsertFact(ctx, db.Fact{
		MemberID: memberID, Metric: "vs_points", Value: float64(points),
		ObservedAt: run.observedAt, PeriodKey: run.periodKey,
		Source: "ocr:vs_ranking", ScreenshotID: row.ScreenshotID,
		Confidence: conf,
	}); err != nil {
		return true, fmt.Errorf("ingest: writing vs_points fact for member %d: %w", memberID, err)
	}
	return true, nil
}

// queueReview mirrors rosterRun.queueReview: records one row that could not
// be confidently resolved (or cleared to write) and counts it. confidence is
// 0 when the reason carries no meaningful blended score (an unparseable
// field, an ambiguous name, no match at all) — QueueReview stores that as
// SQL NULL, not as a claimed zero-confidence read.
func (run *vsRun) queueReview(ctx context.Context, i *Ingester, screenshotID int64, band RowBand, rawText string, candidates []roster.Candidate, reason string, confidence float64) error {
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
