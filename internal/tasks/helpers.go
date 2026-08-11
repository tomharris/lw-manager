package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrTapCapReached reports that a tap-until-gone loop hit its bound. It is
// always a bug: either a disabled control kept matching, or the UI changed.
var ErrTapCapReached = errors.New("tasks: tap cap reached")

// ErrClaimNeverAppeared reports that the radar's Claim All never showed up
// after an execution.
var ErrClaimNeverAppeared = errors.New("tasks: claim never appeared")

// ErrExecuteIgnored reports that Quick Execute was tapped and the game did not
// start an execution — the button stayed exactly where it was.
//
// It is deliberately distinct from ErrClaimNeverAppeared. The two are easy to
// confuse from the outside and were confused for three investigations: a run
// whose tap is swallowed does nothing at all, and then blames the claim for
// failing to appear after an execution that never happened.
var ErrExecuteIgnored = errors.New("tasks: quick execute was ignored")

// tapSettle paces repeated taps on one control so the game registers each,
// and so the tap stream is not a metronome.
//
// It is a var rather than a const only so the device-free tests can collapse
// it: tapUntilGone's cap test spends the full settle on every one of its
// taps, and a suite that must pass with nothing running should not spend its
// wall-clock imitating a human. Nothing outside a test ever assigns it.
var tapSettle = 350 * time.Millisecond

// tapIfPresent taps an anchor when it is on screen, reporting whether it
// was. A missing anchor is an ordinary outcome: no collect bubble means
// nothing accumulated, no help icon means nobody needs help.
//
// Every other error is returned unchanged. In particular ErrWrongScreen is
// not swallowed — being somewhere unexpected is not the same as the button
// being absent, and conflating them is how a task reports success for doing
// nothing.
func tapIfPresent(ctx context.Context, rt *runtime.Ctx, screen, anchorID string) (bool, error) {
	err := rt.Tap(ctx, screen, anchorID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, runtime.ErrAnchorNotFound):
		return false, nil
	default:
		return false, err
	}
}

// dismissRewards clears the rewards celebration if it is playing, and does
// nothing if it is not.
//
// The celebration is a transient animation over its origin screen, not a
// dialog: there is no scrim, no close button, and the origin stays fully
// recognizable throughout. It is dismissed by tapping anywhere — and
// "anywhere" is not a coordinate invariant #3 lets us express, so the tap
// goes to the CONGRATULATIONS banner itself.
//
// The banner is self-gating: it exists only while the animation plays, so a
// missing banner means there is nothing to dismiss and nothing is tapped. The
// rejected alternative was the bottom-left corner, which on every one of these
// screens is a back arrow — fine while the catcher swallows the tap, but if
// the animation has already cleared the same tap navigates away, exiting to
// base mid-task on the radar.
//
// Prefer the anchor whose presence is the condition you are testing for.
func dismissRewards(ctx context.Context, rt *runtime.Ctx, screen string) error {
	_, err := tapIfPresent(ctx, rt, screen, "rewards_banner")
	return err
}

// tapUntilGone taps an anchor until it stops matching, bounded by max.
//
// Ctx.Tap re-verifies the screen and re-matches the anchor on every call, so
// the control becoming unmatchable is the terminating condition and no new
// primitive is needed.
//
// The bound is the backstop for a control that greys out instead of
// vanishing: a disabled button still renders, and NCC will usually still
// match it. Reaching the cap is therefore a defect — the discriminator has
// stopped discriminating — and is returned as an error rather than logged,
// so it lands in task_runs as a failure instead of hiding inside a success.
func tapUntilGone(ctx context.Context, rt *runtime.Ctx, screen, anchorID string, max int) (int, error) {
	for n := 0; n < max; n++ {
		tapped, err := tapIfPresent(ctx, rt, screen, anchorID)
		if err != nil {
			return n, err
		}
		if !tapped {
			return n, nil
		}
		if err := rt.Sleep(ctx, tapSettle, 2*tapSettle); err != nil {
			return n + 1, err
		}
	}
	return max, fmt.Errorf("tasks: %s/%s still matching after %d taps: %w", screen, anchorID, max, ErrTapCapReached)
}

// baseTapTask builds the shape shared by the two tasks that act on the base
// HUD: go to base, tap a badge if it is there. Neither navigates further,
// and for both a missing badge means there is nothing to do.
func baseTapTask(anchorID string) runtime.TaskFunc {
	return func(ctx context.Context, rt *runtime.Ctx) error {
		if err := rt.NavigateTo(ctx, vision.ScreenBase); err != nil {
			return err
		}
		_, err := tapIfPresent(ctx, rt, vision.ScreenBase, anchorID)
		return err
	}
}
