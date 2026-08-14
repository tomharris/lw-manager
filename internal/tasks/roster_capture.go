package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrGroupDidNotExpand reports that a rank-group chevron did not flip after
// being tapped — it never opened, or it never closed back up. A tapped
// header that did not change state yields a perfectly valid capture of the
// wrong thing, the same defect class startExecution guards Quick Execute
// against (radar.go), so this is a hard failure rather than something the
// caller proceeds past (invariant #3: no task acts without a matched screen
// anchor first).
var ErrGroupDidNotExpand = errors.New("tasks: rank group chevron did not flip")

const (
	// maxRankGroups bounds the group loop. It is not a claim about the
	// game: four rank groups exist today and the set is user-edited (see
	// the task brief), so this is only the backstop that turns a
	// non-terminating loop into a fast failure, the same role
	// maxScrollFrames plays for one list.
	maxRankGroups = 8

	// chevronTapAttempts bounds the retries of a chevron tap, matching
	// startExecution's retry count and reasoning: a swallowed tap and a
	// genuine non-response look identical after a single attempt, so the
	// tap is retried before giving up.
	chevronTapAttempts = 3
)

// chevronSettle is how long to let the game react to a chevron tap before
// asking whether it flipped. A var only so the device-free suite can
// collapse it; see TestMain.
var chevronSettle = 900 * time.Millisecond

// memberListRegion and memberRowPitch were measured on the handset. The
// region excludes the sticky alliance footer below the list; the chevron
// anchors' own search region (templates/manifest.yaml) is wider than this —
// it spans the whole scrollable column so a group header can be found
// wherever the groups above it put it — while this is the narrower band
// scrollCapture actually swipes and photographs.
//
// Y1 was 0.42 (y=672 of a 1600px frame), which sits inside the sticky
// rank-group header ("R3 Footloose 15/64", pinned in place while rows scroll
// underneath it): scanning the mean RGB across five separately scrolled
// frames of the same burst for a sharp transition put that header's bar at
// y=650..697, every frame agreeing to within a pixel. 0.44 (y=704) clears
// the header's own bottom edge by 7px rather than cutting through it —
// internal/ingest/roster.go's groupHeaderRegion is measured from the same
// frames and the two constants are consistent by that measurement, not by
// assumption; see its doc comment. This measurably helped ScrollOffset too:
// probe 0's margin on the same frame pair went 0.056 -> 0.117
// (docs/superpowers/specs/evidence/m4-scrolloffset-2026-08-13/).
//
// internal/ingest/roster.go duplicates this Rect rather than importing it —
// see that file's own comment for why — so a further change here must move
// there too.
var memberListRegion = transport.Rect{X1: 0.03, Y1: 0.44, X2: 0.97, Y2: 0.89}

const memberRowPitch = 112

func init() { Register("roster_capture", rosterCapture) }

// roster_capture walks the alliance member list, one rank group at a time.
//
// Group names are user-editable and the group set varies (an alliance seen
// three weeks apart had different names and a different count), so this
// deliberately never identifies which group it is looking at. Every
// collapsed chevron is a valid next target — Match returns the
// best-scoring placement rather than the topmost, and that is fine because
// the loop only cares whether one remains, not which one it is. Rank
// attribution moves to ingest, which reads it from each frame's own sticky
// header rather than trusting a label this task would otherwise have to
// assert.
func rosterCapture(ctx context.Context, rt *runtime.Ctx) error {
	if err := rt.NavigateTo(ctx, vision.ScreenAlliance); err != nil {
		return err
	}
	// The alliance frame carries the roster's reconciliation ground truth
	// ("Members: 96/100"), the tag, name, and leader — captured once, not
	// per group. It is recorded as this capture's own frame, tagged with
	// vision.AllianceSummaryGroupKey rather than left to sit only in the
	// generic screenshots/task_runs tables (the M4 task-11 gap this closes):
	// ingest reads that tag to find it and, just as importantly, to skip it
	// when segmenting rows — it is not a list screen.
	allianceScreenshotID, err := rt.Capture(ctx, vision.ScreenAlliance)
	if err != nil {
		return err
	}
	if err := rt.NavigateTo(ctx, vision.ScreenAllianceMembers); err != nil {
		return err
	}

	all := []ScrolledFrame{
		{ScreenshotID: allianceScreenshotID, GroupKey: vision.AllianceSummaryGroupKey},
	}
	allComplete := true
	for i := 0; i < maxRankGroups; i++ {
		if err := rt.CheckKillSwitch(ctx); err != nil {
			return err
		}
		present, err := rt.Sees(ctx, vision.ScreenAllianceMembers, "chevron_collapsed")
		if err != nil {
			return err
		}
		if !present {
			break // every group has been visited
		}

		if err := openGroup(ctx, rt); err != nil {
			return err
		}

		frames, complete, err := scrollCapture(ctx, rt, ScrollSpec{
			Screen:    vision.ScreenAllianceMembers,
			Region:    memberListRegion,
			Pitch:     memberRowPitch,
			SwipeFrac: 0.35, // 252px swipe at the region's current height; run 365 measured 247-393px travel from it (16 real pairs)
			// GroupKey stays empty: which rank a frame belongs to is read
			// from the frame's own sticky header at ingest, not asserted
			// here (see the doc comment above).
		})
		if err != nil {
			return fmt.Errorf("tasks: capturing roster group %d: %w", i, err)
		}
		all = append(all, frames...)
		if !complete {
			allComplete = false
		}

		if err := closeGroup(ctx, rt); err != nil {
			return err
		}
	}

	return recordFrames(ctx, rt, "roster", all, allComplete)
}

// openGroup taps the collapsed chevron and confirms the group opened.
func openGroup(ctx context.Context, rt *runtime.Ctx) error {
	return toggleChevron(ctx, rt, "chevron_collapsed", "chevron_expanded")
}

// closeGroup taps the expanded chevron and confirms the group closed again,
// so the next loop iteration's chevron_collapsed check is not confused by a
// group this task itself left open.
func closeGroup(ctx context.Context, rt *runtime.Ctx) error {
	return toggleChevron(ctx, rt, "chevron_expanded", "chevron_collapsed")
}

// toggleChevron taps tapAnchor and confirms the flip by wantAnchor's
// presence, retrying the tap up to chevronTapAttempts times before giving
// up — the same shape and count as startExecution's retry of Quick Execute.
//
// A tapped header that did not open (or did not close) yields a perfectly
// valid capture of the wrong thing, which is the same defect class a
// swallowed tap is elsewhere in this codebase: the only local proof the
// game accepted the tap is the chevron actually flipping, so that is what
// is waited for, and a tap that does not produce it is retried rather than
// assumed.
func toggleChevron(ctx context.Context, rt *runtime.Ctx, tapAnchor, wantAnchor string) error {
	for i := 0; i < chevronTapAttempts; i++ {
		if err := rt.Tap(ctx, vision.ScreenAllianceMembers, tapAnchor); err != nil {
			return err
		}
		if err := rt.Sleep(ctx, chevronSettle, 2*chevronSettle); err != nil {
			return err
		}
		seen, err := rt.Sees(ctx, vision.ScreenAllianceMembers, wantAnchor)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}
	}
	return fmt.Errorf("tasks: tapped %s %d times, %s never appeared: %w",
		tapAnchor, chevronTapAttempts, wantAnchor, ErrGroupDidNotExpand)
}

// recordFrames converts scroll-captured frames into runtime.CaptureFrameRef
// and hands them to rt.RecordCapture as one capture per run.
//
// Seq is renumbered sequentially here rather than trusted from the input:
// scrollCapture numbers ScrolledFrame.Seq from zero on every call, so
// concatenating several groups' frames as-is would produce repeated Seq
// values, and capture_frames has UNIQUE (capture_id, seq).
func recordFrames(ctx context.Context, rt *runtime.Ctx, route string, frames []ScrolledFrame, complete bool) error {
	refs := make([]runtime.CaptureFrameRef, len(frames))
	for i, f := range frames {
		refs[i] = runtime.CaptureFrameRef{
			ScreenshotID: f.ScreenshotID,
			Seq:          i,
			OffsetPx:     f.OffsetPx,
			GroupKey:     f.GroupKey,
		}
	}
	return rt.RecordCapture(ctx, route, refs, complete)
}
