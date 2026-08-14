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
	// recon found the roster's 0.35 (263px swipe -> 326-359px travel) left
	// the VS ranking's deepest probe with too little headroom (665px travel
	// against probe 2's 630px limit — see internal/vision/scroll.go's
	// ScrollOffset doc comment for what a probe's own limit means), so VS
	// uses 0.25 instead. A single shared literal would have coupled that
	// fix to the roster's already-measured, already-working behaviour.
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
func usableHeight(rt *runtime.Ctx, spec ScrollSpec) (int, error) {
	size := rt.Resolution()
	h := int((spec.Region.Y2 - spec.Region.Y1) * float64(size.Y))
	if h <= spec.Pitch {
		return 0, fmt.Errorf("tasks: region on %s is %d px, not taller than one %d px row", spec.Screen, h, spec.Pitch)
	}
	return h - spec.Pitch, nil
}

// swipeOnce performs one measured-size swipe inside the region, travelling
// spec.SwipeFrac of the region's own height before fling. 300px over 800ms
// (0.35 of the roster's region) was measured on the handset at ~512px of
// travel — about 48% overlap against a 990px viewport — where 700px over
// 300ms travelled ~1504px and skipped rows.
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
