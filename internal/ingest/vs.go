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
// height (Y) — recon-estimated from frame 05
// (docs/superpowers/specs/evidence/m4-recon-2026-08-12/05-weekly-your-alliance-checked.png).
// Unverified pending a device session, same caveat as roster.go's field
// fractions: no unit test depends on their accuracy, since ocr.FakeEngine
// ignores the pixels it is handed.
const (
	vsNameXFrac0, vsNameXFrac1     = 0.26, 0.63
	vsPointsXFrac0, vsPointsXFrac1 = 0.65, 0.97

	vsNameYFrac0, vsNameYFrac1     = 0.05, 0.50
	vsPointsYFrac0, vsPointsYFrac1 = 0.25, 0.75
)

// See roster.go's Spec-value doc comment: these MinConf values document each
// field's OCR difficulty but are advisory, not enforced — factConfidenceGate
// is the floor actually applied, against each field's blended (name-match x
// OCR) confidence.
var (
	vsNameSpec   = ocr.Spec{MinConf: 0.4}
	vsPointsSpec = ocr.Spec{Charset: "0123456789,", MinConf: 0.6}
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
type VSResult struct {
	Matched, Queued, Zeroed int
	Status                  string
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

	// Absence means zero, but only on a complete capture: the weekly ranking
	// lists only members with a nonzero score (recon measured 94 ranked rows
	// against 96 alliance members), so a member missing from a complete
	// capture genuinely scored nothing. On a partial capture, absence and
	// truncation are indistinguishable, so this writes no zeroes at all — a
	// capture wrongly treated as complete would otherwise silently zero real
	// scores for exactly the members hardest to see.
	if capture.Status == "complete" {
		for _, m := range members {
			if scored[m.ID] {
				continue
			}
			if _, err := i.store.InsertFact(ctx, db.Fact{
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
	nameRes, err := i.readField(ctx, img, fieldRect(band, img, vsNameXFrac0, vsNameXFrac1, vsNameYFrac0, vsNameYFrac1), vsNameSpec)
	if err != nil {
		return false, err
	}
	pointsRes, err := i.readField(ctx, img, fieldRect(band, img, vsPointsXFrac0, vsPointsXFrac1, vsPointsYFrac0, vsPointsYFrac1), vsPointsSpec)
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
		if _, err := i.store.InsertFact(ctx, db.Fact{
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
