package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/vision"
)

// claimPollBudget and claimPollInterval bound the wait for the claim control
// after a *confirmed* execution. Both are vars only so the device-free suite
// can collapse them; see TestMain.
//
// The budget is wall clock rather than an attempt count, which is the honest
// unit: the retired bound was eight attempts, and since every attempt costs a
// screencap plus a full recognition pass — roughly 9s on a moto g play 2024 —
// it silently meant whatever the handset's speed made it mean.
//
// Ninety seconds is generous against what was actually measured. In a burst
// capture of a succeeding run the rewards and the collect tray were on screen
// within 3s of the tap landing. The previous four minutes was fitted to a
// phantom: runs were failing because the execute tap had been swallowed, so no
// budget could ever have been long enough, and the only thing the extra time
// bought was a slower failure that then blamed the wrong half of the task.
var (
	claimPollBudget   = 90 * time.Second
	claimPollInterval = 700 * time.Millisecond
)

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

	started, err := startExecution(ctx, rt)
	if err != nil {
		return err
	}
	if !started {
		return nil // no targets, or the stamina dialog was backed out of
	}
	return claimWhenReady(ctx, rt)
}

// executeTapAttempts bounds the retries of Quick Execute. Three is generous:
// every attempt costs a screencap plus a full recognition pass either side of
// it, so the game gets the better part of a minute to accept one of them.
const executeTapAttempts = 3

// executeSettle is how long to let the game react before asking whether it
// accepted the tap. A var only so the device-free suite can collapse it.
//
// The observed transition is fast: in the burst capture of a succeeding run the
// button was gone within 3s of the tap landing. The probe that follows costs a
// screencap and a recognition pass on top, so a short settle is ample and the
// window for tapping twice into one accepted execution stays small.
var executeSettle = 1500 * time.Millisecond

// startExecution taps Quick Execute and confirms the game actually started an
// execution, reporting whether one is now underway.
//
// This exists because `Tap` succeeding means "I tapped", not "the game acted",
// and radar spent three investigations conflating the two. Run 356 tapped, was
// ignored, and then waited out the entire claim budget — 260s — for the reward
// of an execution that never started, while stamina rose by one. The button
// disappearing is the only local proof available that the tap was accepted, so
// that is what is waited for, and a tap that is not accepted is retried rather
// than assumed.
//
// The stamina dialog stays an ordinary outcome. That is what previously made a
// retry loop unsafe — Quick Execute renders enabled while stating a cost the
// account cannot pay, so a loop terminating on the button's absence would never
// terminate — and it is why the prompt is checked on every pass rather than
// only by the claim poll.
func startExecution(ctx context.Context, rt *runtime.Ctx) (bool, error) {
	tapped := false
	for i := 0; i < executeTapAttempts; i++ {
		present, err := rt.Sees(ctx, vision.ScreenRadar, "quick_execute_button")
		if err != nil {
			return false, err
		}
		if !present {
			// Before any tap this means there was nothing to execute. After
			// one it means the game took it.
			return tapped, nil
		}
		if err := rt.Tap(ctx, vision.ScreenRadar, "quick_execute_button"); err != nil {
			return false, err
		}
		tapped = true

		if err := rt.Sleep(ctx, executeSettle, 2*executeSettle); err != nil {
			return false, err
		}
		r, err := rt.CurrentScreen(ctx)
		if err != nil {
			return false, err
		}
		if r.Screen == vision.ScreenStaminaPrompt {
			// Nothing in the dialog is ever tapped: it spends real currency.
			// Leaving is the whole correct interaction, and stamina
			// regenerates before the next run.
			return false, rt.NavigateTo(ctx, vision.ScreenRadar)
		}
	}
	return false, fmt.Errorf("tasks: Quick Execute tapped %d times and no execution started: %w",
		executeTapAttempts, ErrExecuteIgnored)
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
	deadline := time.Now().Add(claimPollBudget)
	for {
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
		// Checked after the poll, as WaitFor does, so a budget collapsed to
		// nothing still buys one look.
		if time.Now().After(deadline) {
			break
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
