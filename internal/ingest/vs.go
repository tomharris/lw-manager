package ingest

import (
	"context"
	"fmt"
	"image"
	"log/slog"
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
// belt to the identity cross-check's braces below.
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
// The points field has no retry at all, and the asymmetry is the point: a name
// that reads badly fails to match a known roster and goes to review, while a
// number has no such guard — a raw-line retry on a crop that caught
// neighbouring content could manufacture a plausible value, which is exactly
// the failure the vsPointsSpec charset comment above describes.
var (
	vsName      = readPlan{spec: vsNameSpec, opts: vsNameOptions}
	vsNameRetry = readPlan{
		spec: ocr.Spec{MinConf: vsNameSpec.MinConf, PSM: ocr.PSMRawLine, Languages: vsNameLanguages},
		opts: vision.Options{SkipEqualize: true, SkipThreshold: true, UpscaleFactor: 4},
	}
)

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
	Status                                string
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

	// scored is the identity-dedupe set: which members already have a
	// vs_points fact written (or attempted) from this capture. It is also
	// the belt-and-braces cross-check's other half — see the disagreement
	// check below.
	scored := map[int64]bool{}
	var matchedRowCount, totalParsed int
	var lastFrameShotID int64
	contentY, lastRowY := 0, -1
	havePrev := false

	for _, frame := range frames {
		img, err := i.loadFrame(ctx, frame.ScreenshotID)
		if err != nil {
			return VSResult{}, fmt.Errorf("ingest: loading screenshot %d: %w", frame.ScreenshotID, err)
		}
		lastFrameShotID = frame.ScreenshotID

		if havePrev {
			contentY += frame.OffsetPx
		}
		havePrev = true

		bands, err := SegmentRows(img, vsListRegion, vsRowPitch)
		if err != nil {
			return VSResult{}, fmt.Errorf("ingest: segmenting screenshot %d: %w", frame.ScreenshotID, err)
		}

		regionTop := int(vsListRegion.Y1 * float64(img.Bounds().Dy()))
		for _, band := range bands {
			rowY := contentY + (band.Y0 - regionTop)
			if lastRowY >= 0 && rowY <= lastRowY+vsRowPitch/2 {
				continue // geometric duplicate; OCR never runs on it
			}
			lastRowY = rowY
			totalParsed++

			matched, err := run.processRow(ctx, i, img, band, frame.ScreenshotID, members, scored)
			if err != nil {
				return VSResult{}, err
			}
			if matched {
				matchedRowCount++
			} else {
				run.res.Unidentified++
			}
		}
	}

	// The identity cross-check the roster route deliberately omitted (see
	// roster.go's dedupe-site comment, and CLAUDE.md: it exists to catch the
	// pinned self row). Geometric dedupe only compares a row's position to
	// the one immediately before it, so it cannot see the same member
	// matched at two unrelated positions in the same capture.
	// matchedRowCount counts every row whose name auto-accepted a match,
	// including a repeat; len(scored) counts only the distinct members that
	// made it past deduplication. A mismatch is not silently resolved — it
	// is flagged, so a problem bigger than the one known, structurally
	// excluded pinned row does not go unnoticed.
	if matchedRowCount != len(scored) {
		slog.WarnContext(ctx, "ingest: vs ranking identity cross-check found a disagreement between geometric and identity match counts",
			"capture_id", captureID, "geometric_matches", matchedRowCount, "identity_matches", len(scored))
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
		for _, m := range members {
			if scored[m.ID] {
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

// processRow reads one row's name and points fields, matches the name, and
// routes the row to a fact, a duplicate no-op, or the review queue. It never
// creates a member: the roster route is the only writer of that table, so a
// name that matches nothing here goes to review, full stop — minting a
// member from a misread ranking row would corrupt the very count
// reconciliation depends on.
//
// Returns whether the row's name auto-accepted a match at all, independent
// of whether that member had already been scored this capture — a duplicate
// still counts toward the caller's matchedRowCount, or the disagreement it
// represents would cancel itself out and the cross-check would never fire.
func (run *vsRun) processRow(ctx context.Context, i *Ingester, img image.Image, band RowBand, screenshotID int64, members []roster.Member, scored map[int64]bool) (bool, error) {
	nameRes, err := i.readFieldWithRetry(ctx, img, fieldRect(band, img, vsNameXFrac0, vsNameXFrac1, vsNameYFrac0, vsNameYFrac1), vsName, vsNameRetry)
	if err != nil {
		return false, err
	}
	pointsRes, err := i.readField(ctx, img, fieldRect(band, img, vsPointsXFrac0, vsPointsXFrac1, vsPointsYFrac0, vsPointsYFrac1), vsPointsSpec, vsPointsOptions)
	if err != nil {
		return false, err
	}

	candidates := roster.Rank(nameRes.Text, members)
	switch {
	case len(candidates) > 0 && candidates[0].Score >= roster.AutoAccept:
		memberID := candidates[0].MemberID
		if scored[memberID] {
			// Same member, a different screen position: the pinned self row
			// (or a genuine bug surfacing elsewhere). Deduplicate by member
			// id — do not write a second fact for it.
			return true, nil
		}
		scored[memberID] = true
		run.res.Matched++

		matchNorm := float64(candidates[0].Score) / 100.0
		points, perr := ParsePoints(pointsRes.Text)
		if perr != nil {
			return true, run.queueReview(ctx, i, screenshotID, band, pointsRes.Text, nil, "unparseable_points", 0)
		}
		conf := min(matchNorm, pointsRes.Confidence)
		if conf < factConfidenceGate {
			return true, run.queueReview(ctx, i, screenshotID, band, pointsRes.Text, nil, "low_confidence_points", conf)
		}
		// UpsertFact, not InsertFact, for the reason IngestRoster's
		// writeFacts already documents at length: observed_at is pinned to
		// the capture's own started_at, so re-running ingest over this same
		// capture recomputes the identical (member_id, metric, period_key,
		// source, observed_at) key and a plain INSERT rejects it. That is not
		// hypothetical here — resolving a review in studio tells the operator
		// to ingest the capture again, so the second run is the whole point
		// of the review loop, and it died on a unique-constraint violation
		// before it could correct anything.
		if _, _, err := i.store.UpsertFact(ctx, db.Fact{
			MemberID: memberID, Metric: "vs_points", Value: float64(points),
			ObservedAt: run.observedAt, PeriodKey: run.periodKey,
			Source: "ocr:vs_ranking", ScreenshotID: screenshotID,
			Confidence: conf,
		}); err != nil {
			return true, fmt.Errorf("ingest: writing vs_points fact for member %d: %w", memberID, err)
		}
		return true, nil

	case len(candidates) > 0 && candidates[0].Score >= roster.ReviewFloor:
		return false, run.queueReview(ctx, i, screenshotID, band, nameRes.Text, candidates, "ambiguous_name_match", 0)

	default:
		return false, run.queueReview(ctx, i, screenshotID, band, nameRes.Text, candidates, "no_confident_match", 0)
	}
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
