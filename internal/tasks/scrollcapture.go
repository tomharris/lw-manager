package tasks

import (
	"context"
	"errors"
	"fmt"
	"image"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrScrollOvershot reports that the list moved further than the visible
// region between two frames, so rows passed by without ever being
// photographed. No dedupe can recover them, which is why this is an error and
// not a warning.
//
// Belt-and-braces, not the primary defence: vision.ScrollOffset's own
// geometry check refuses a candidate before it ever reaches usableHeight's
// comparison here, for both routes' actual numbers. Roster (Pitch 112,
// regionH 720 at Y1=0.44): ScrollOffset's ceiling is 548px against a usable
// of 608px. VS (Pitch 128, regionH 984): ceiling 748px against a usable of
// 856px. Either way the vision-side refusal (ErrOffsetUncertain) fires
// first — which is the right order, since it is the stricter,
// measurement-integrity check, and reordering it behind this one would let
// an unmeasurable candidate reach a comparison that assumes it is real. The
// consequence is that this branch is not dead code (a different
// Pitch/regionH ratio, Pitch < 0.24*regionH, would still reach it, and
// TestScrollCaptureFlagsAnOvershoot still drives it directly) but it is
// unreachable in production today: a genuine overswipe on either route
// surfaces as vision.ErrOffsetUncertain, wrapped by the "measuring scroll
// frame" error below, not as this sentinel. See usableHeight's doc comment
// for the same numbers from the other side.
var ErrScrollOvershot = errors.New("tasks: scroll moved further than the visible region")

const (
	// maxScrollFrames caps a single list. R3 holds 64 members at roughly four
	// rows a swipe, so ~16 frames; 40 is comfortably double and turns a
	// non-converging loop into a fast failure rather than a hang.
	maxScrollFrames = 40
	// zeroOffsetRetries is how many times a zero offset is retried before the
	// bottom is believed. Three, matching startExecution, and for the same
	// reason: a swallowed gesture and a real end look identical.
	zeroOffsetRetries = 3
)

// swipeSettleMin/Max are the pause after a swipe, before capturing. Fling has
// to finish or the offset measures a moving list. Vars, not consts — like
// tapSettle, tabSettle and executeSettle, only so the device-free suite can
// collapse them; see TestMain. A scroll can retry up to zeroOffsetRetries
// times per frame across as many as maxScrollFrames frames, so at the real
// 900-1400ms this would add the better part of a minute to `go test
// ./internal/tasks/` for no signal a real device is not already providing.
var (
	swipeSettleMin = 900 * time.Millisecond
	swipeSettleMax = 1400 * time.Millisecond
)

// ScrollSpec describes one scrollable list.
type ScrollSpec struct {
	// Screen is the recognized screen this list lives on. Every frame is
	// verified against it, so a mid-scroll navigation away fails rather than
	// silently capturing something else.
	Screen string
	// Region is the scrollable area, excluding sticky headers above it and any
	// pinned row below it.
	Region transport.Rect
	// Pitch is the expected row height in pixels at the reference resolution.
	Pitch int
	// SwipeFrac is how much of the region's own height one swipe travels,
	// before fling. Measured per list rather than shared as one constant:
	// recon found the roster's 0.35 left the VS ranking's deepest probe with
	// too little headroom (665px travel against probe 2's 630px limit — see
	// internal/vision/scroll.go's ScrollOffset doc comment for what a
	// probe's own limit means), so VS uses 0.25 instead. A single shared
	// literal would have coupled that fix to the roster's already-measured,
	// already-working behaviour.
	//
	// The roster's own 0.35 was originally measured as a 263px swipe (0.35
	// of a 0.47-tall region) producing 326-359px of travel. Both numbers
	// moved when memberListRegion.Y1 shifted to clear the sticky header
	// (0.42 -> 0.44, shrinking the region to 0.45 of the frame): the same
	// 0.35 is now a 252px swipe, and device run 365 measured 247-393px of
	// travel from it across 16 real pairs — still comfortably inside the
	// 548px measurement ceiling (ErrScrollOvershot's doc comment), so 0.35
	// did not need to move with the region, but the number this comment
	// once quoted did.
	SwipeFrac float64
	// GroupKey labels every frame, carrying the rank group on the roster route.
	GroupKey string
}

// ScrolledFrame is one captured frame and the offset that reached it.
type ScrolledFrame struct {
	ScreenshotID int64
	Seq          int
	OffsetPx     int
	GroupKey     string
}

// scrollCapture swipes through a list, capturing a frame per step and
// measuring how far the content actually moved. It returns the frames and
// whether the bottom was proven.
//
// The measurement is the point. Recon found fling roughly doubles a gesture,
// so a loop that trusts its swipe distance skips rows while every frame still
// looks valid — the failure is invisible downstream.
func scrollCapture(ctx context.Context, rt *runtime.Ctx, spec ScrollSpec) ([]ScrolledFrame, bool, error) {
	usable, err := usableHeight(rt, spec)
	if err != nil {
		return nil, false, err
	}

	var frames []ScrolledFrame
	prev, err := rt.Screenshot(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("tasks: capturing scroll frame 0: %w", err)
	}
	if err := captureFrame(ctx, rt, spec, 0, 0, prev, &frames); err != nil {
		return nil, false, err
	}

	zeroes := 0
	for seq := 1; seq < maxScrollFrames; seq++ {
		if err := rt.CheckKillSwitch(ctx); err != nil {
			return frames, false, err
		}
		if err := swipeOnce(ctx, rt, spec); err != nil {
			return frames, false, err
		}

		cur, err := rt.Screenshot(ctx)
		if err != nil {
			return frames, false, fmt.Errorf("tasks: capturing scroll frame %d: %w", seq, err)
		}
		offset, err := vision.ScrollOffset(prev, cur, spec.Region)
		if err != nil {
			// No retry here, considered and rejected: task 26 investigated
			// exactly this failure (two real vs_capture runs died on this
			// line) and found it was not a transient bad frame. Both errors
			// named the score check specifically, meaning reach and
			// agreement — the checks that actually catch a bad frame, an
			// animation, or a mid-scroll capture — had already passed; the
			// candidate was a real, agreed-upon placement that a
			// miscalibrated offsetMinScore rejected anyway. Re-capturing and
			// re-measuring the same static list content would have scored
			// about the same and failed again. The fix was recalibrating
			// vision.ScrollOffset's floor (see its doc comment), not
			// retrying around it — a retry would have cost time without
			// fixing anything, and would have hidden the real defect behind
			// an occasional extra pass.
			return frames, false, fmt.Errorf("tasks: measuring scroll frame %d on %s: %w", seq, spec.Screen, err)
		}

		switch {
		case offset == 0:
			zeroes++
			if zeroes >= zeroOffsetRetries {
				return frames, true, nil
			}
			continue
		case offset > usable:
			return frames, false, fmt.Errorf("tasks: %s moved %d px against a usable region of %d px: %w",
				spec.Screen, offset, usable, ErrScrollOvershot)
		}
		zeroes = 0

		// cur is exactly the frame the offset above was measured against —
		// captureFrame verifies and stores that same image, not a fresh one,
		// so the ScreenshotID it records and the pixels OffsetPx describes
		// are never two different captures of a screen that is not static
		// (the ranking header's scrolling banner, the roster's live "Online"
		// badges).
		if err := captureFrame(ctx, rt, spec, seq, offset, cur, &frames); err != nil {
			return frames, false, err
		}
		prev = cur
	}
	// Ran out of frames without a proven bottom.
	return frames, false, nil
}

// usableHeight is the region's height less one row pitch: the furthest the
// list may travel while still leaving every row photographed somewhere.
//
// This is compared against vision.ScrollOffset's own result to raise
// ErrScrollOvershot, but for both real routes ScrollOffset's geometry check
// refuses a candidate before it ever gets this far: roster's usable is
// 608px against a 548px measurement ceiling, VS's is 856px against 748px.
// See ErrScrollOvershot's doc comment for the full arithmetic and why the
// stricter check firing first is correct rather than something to reorder
// around.
func usableHeight(rt *runtime.Ctx, spec ScrollSpec) (int, error) {
	// SwipeFrac <= 0 belongs in this same validation, not left to fail
	// downstream: swipeOnce's `to` collapses onto `from` (a zero-distance
	// swipe), so every measured offset reads exactly 0, zeroOffsetRetries is
	// reached on schedule, and scrollCapture returns (1 frame, complete =
	// true, nil) — a capture truncated to its very first frame, reported as
	// having proven the bottom. That is indistinguishable from a genuinely
	// one-row list without checking the frame count, which is exactly the
	// silently-truncated-but-reported-complete failure invariant #4 exists
	// to catch. All three current call sites set SwipeFrac explicitly, so
	// this is a latent guard, not a live fix — but a ScrollSpec literal that
	// omits the field is a compile-time-valid zero value, and nothing else
	// stops it.
	if spec.SwipeFrac <= 0 {
		return 0, fmt.Errorf("tasks: %s has a non-positive SwipeFrac (%.3f): a swipe that travels nothing would report a truncated capture as complete", spec.Screen, spec.SwipeFrac)
	}
	size := rt.Resolution()
	h := int((spec.Region.Y2 - spec.Region.Y1) * float64(size.Y))
	if h <= spec.Pitch {
		return 0, fmt.Errorf("tasks: region on %s is %d px, not taller than one %d px row", spec.Screen, h, spec.Pitch)
	}
	return h - spec.Pitch, nil
}

// swipeOnce performs one measured-size swipe inside the region, travelling
// spec.SwipeFrac of the region's own height before fling. The roster's own
// 0.35 is currently a 252px swipe over 800ms (0.35 of the region's 0.45-tall
// height at Y1=0.44), which device run 365 measured producing 247-393px of
// travel across 16 real frame pairs — comfortably inside the 548px
// measurement ceiling (see ErrScrollOvershot). A much harder swipe overshoots
// badly instead of merely travelling further: 700px over 300ms was measured
// at ~1504px of travel against a ~990px viewport, skipping rows entirely —
// which is why SwipeFrac stays a small, measured-per-list fraction rather
// than a bigger number chosen for speed.
func swipeOnce(ctx context.Context, rt *runtime.Ctx, spec ScrollSpec) error {
	midX := (spec.Region.X1 + spec.Region.X2) / 2
	span := spec.Region.Y2 - spec.Region.Y1
	from := transport.Norm{X: midX, Y: spec.Region.Y1 + span*0.75}
	to := transport.Norm{X: midX, Y: spec.Region.Y1 + span*0.75 - span*spec.SwipeFrac}
	if err := rt.Swipe(ctx, from, to); err != nil {
		return fmt.Errorf("tasks: swiping %s: %w", spec.Screen, err)
	}
	return rt.Sleep(ctx, swipeSettleMin, swipeSettleMax)
}

// captureFrame verifies img shows spec.Screen and stores that exact image —
// never a second, separately-taken screenshot.
//
// The first implementation of this helper took three screenshots to do this
// job (a CurrentScreen check, Capture's own internal re-verification, and a
// trailing Screenshot to hand back as the next prev), and the loop above took
// a fourth and fifth of its own to perform the actual offset measurement. On
// a genuinely static screen the five would all show identical pixels, but
// neither of this route's two target screens is static — the ranking screen
// carries a scrolling announcement banner in its header, and the roster
// carries live "Online" badges — so the frame that earned a ScreenshotID
// could differ from the one OffsetPx was measured against by however much
// moved in the gap between captures. rt.VerifyFrame and rt.StoreFrame both
// operate on the one image the caller already holds, so there is only ever
// one screenshot per captured frame, and it is provably the same one the
// offset describes.
func captureFrame(ctx context.Context, rt *runtime.Ctx, spec ScrollSpec, seq, offset int, img image.Image, out *[]ScrolledFrame) error {
	if err := rt.VerifyFrame(ctx, spec.Screen, img); err != nil {
		return fmt.Errorf("tasks: verifying scroll frame %d: %w", seq, err)
	}
	id, err := rt.StoreFrame(ctx, spec.Screen, img)
	if err != nil {
		return fmt.Errorf("tasks: storing scroll frame %d: %w", seq, err)
	}
	*out = append(*out, ScrolledFrame{ScreenshotID: id, Seq: seq, OffsetPx: offset, GroupKey: spec.GroupKey})
	return nil
}
