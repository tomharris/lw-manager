package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/vision"
)

// claimPollAttempts and claimPollInterval bound the wait for Claim All,
// which the game reveals a few seconds after an execution. The interval is a
// var only so the device-free suite can collapse it; see TestMain.
const claimPollAttempts = 8

var claimPollInterval = 700 * time.Millisecond

func init() { Register("radar", radarPass) }

// radar claims any banked rewards, then runs one execution and claims its
// rewards too.
//
// This replaces the earlier radar_quick and radar_claim, which assumed the two
// halves were independent. They are not: only one of the buttons is on screen
// at a time, and a pending Claim All blocks the next Quick Execute.
//
// Claim-first is load-bearing. A run that skipped claiming would find Quick
// Execute absent, correctly conclude there was nothing to execute, and stop —
// while sitting on banked rewards it could have claimed and then executed.
//
// There is no sweep loop. Quick Execute renders enabled while stating a
// stamina cost the account cannot pay, so a loop terminating on the button's
// absence would never terminate on insufficient stamina. One pass per run
// cannot spiral, and the next run is three hours away.
//
// Both radar states are the same screen — `radar` is identified by its header,
// and which button is present says what state it is in. That dodges the
// disabled-control trap by construction: each state is identified by something
// that is there, never by something that is not.
func radarPass(ctx context.Context, rt *runtime.Ctx) error {
	if err := rt.NavigateTo(ctx, vision.ScreenRadar); err != nil {
		return err
	}

	// Clear anything banked first: a pending claim blocks the next execution.
	if err := claimAndDismiss(ctx, rt); err != nil {
		return err
	}

	executed, err := tapIfPresent(ctx, rt, vision.ScreenRadar, "quick_execute_button")
	if err != nil {
		return err
	}
	if !executed {
		return nil // no targets
	}
	return claimWhenReady(ctx, rt)
}

// claimAndDismiss taps Claim All if it is showing and clears the celebration.
// Absent means nothing is banked, which is ordinary.
func claimAndDismiss(ctx context.Context, rt *runtime.Ctx) error {
	claimed, err := tapIfPresent(ctx, rt, vision.ScreenRadar, "claim_all_button")
	if err != nil || !claimed {
		return err
	}
	return dismissRewards(ctx, rt, vision.ScreenRadar)
}

// claimWhenReady polls for Claim All after an execution, then claims it.
//
// WaitFor cannot express this wait: it polls for a *screen*, and the screen
// does not change — it is `radar` before and after. Only the anchor's arrival
// marks the transition, and Ctx.Tap re-matches on every call, so a bounded
// loop of tapIfPresent is exactly "wait until it appears, then tap it".
//
// The loop also watches for the stamina dialog. Quick Execute renders enabled
// when stamina is short, and tapping it opens a buy/refill prompt rather than
// starting an execution. Leaving is the entire correct interaction: the dialog
// spends real currency, so nothing inside it is ever tapped, and stamina
// regenerates on its own before the next run. That makes it an ordinary
// outcome rather than a failure.
func claimWhenReady(ctx context.Context, rt *runtime.Ctx) error {
	for i := 0; i < claimPollAttempts; i++ {
		r, err := rt.CurrentScreen(ctx)
		if err != nil {
			return err
		}
		if r.Screen == vision.ScreenStaminaPrompt {
			// Back out via the graph's escape edge. Nothing here is tapped.
			return rt.NavigateTo(ctx, vision.ScreenRadar)
		}
		if r.Screen == vision.ScreenRadar {
			claimed, err := tapIfPresent(ctx, rt, vision.ScreenRadar, "claim_all_button")
			if err != nil {
				return err
			}
			if claimed {
				return dismissRewards(ctx, rt, vision.ScreenRadar)
			}
		}
		if err := rt.Sleep(ctx, claimPollInterval, 2*claimPollInterval); err != nil {
			return err
		}
	}
	// Insufficient stamina now exits via the prompt above, so a Claim All that
	// never arrives after a genuine execution means something is actually
	// wrong. The signal is worth keeping.
	return fmt.Errorf("tasks: Claim All never appeared after an execution: %w", ErrClaimNeverAppeared)
}
