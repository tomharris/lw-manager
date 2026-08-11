package tasks

import (
	"context"
	"errors"
	"image"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/vision"
)

// The four radar states, all of them the same screen. Which button is drawn
// is what says which state it is in — never which button is missing, because
// an anchor can report presence and nothing else.
func radarStates() (exec, claim, celebrating, done image.Image) {
	return runtimetest.FrameWithout(vision.ScreenRadar, "claim_all_button", "rewards_banner"),
		runtimetest.FrameWithout(vision.ScreenRadar, "quick_execute_button", "rewards_banner"),
		runtimetest.FrameWithout(vision.ScreenRadar, "quick_execute_button", "claim_all_button"),
		runtimetest.FrameWithout(vision.ScreenRadar, "quick_execute_button", "claim_all_button", "rewards_banner")
}

// toRadar is the two base frames NavigateTo spends before the tap lands:
// the opening CurrentScreen and the tap verify. The WaitFor and the arrival
// CurrentScreen both read radar frames, so they come from the caller's state.
func toRadar() []image.Image {
	base := runtimetest.Frame(vision.ScreenBase)
	return []image.Image{base, base}
}

// One pass: execute, wait for Claim All to appear, claim, dismiss.
func TestRadarExecutesThenClaims(t *testing.T) {
	exec, claim, celebrating, done := radarStates()

	frames := toRadar()
	// WaitFor, arrival CurrentScreen, the banked-claim check, the Sees probe
	// that finds Quick Execute, and the execute tap itself.
	frames = append(frames, rep(exec, 5)...)
	// The post-tap CurrentScreen, then the Sees probe that finds the button
	// gone — which is the proof the game accepted the tap.
	frames = append(frames, rep(claim, 2)...)
	// The claim poll's CurrentScreen, then the claim tap.
	frames = append(frames, rep(claim, 2)...)
	frames = append(frames, celebrating) // the banner is up: dismiss it
	frames = append(frames, rep(done, 2)...)
	rt, tr := ctxFor(t, frames...)

	fn, ok := Get("radar")
	if !ok {
		t.Fatal("radar not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("radar: %v", err)
	}
	if taps := countTaps(tr); taps != 4 {
		t.Errorf("got %d taps, want 4 (navigation, execute, claim, dismiss)", taps)
	}
}

// Rewards banked from a previous run block the next Quick Execute, so they
// are claimed before anything else is attempted. A run that executed first
// would find no button and correctly conclude there was nothing to do —
// while sitting on the claim that was causing it.
func TestRadarClaimsBankedRewardsBeforeExecuting(t *testing.T) {
	exec, claim, celebrating, _ := radarStates()

	frames := toRadar()
	frames = append(frames, rep(claim, 3)...) // WaitFor, CurrentScreen, the banked claim
	frames = append(frames, celebrating)      // its celebration
	frames = append(frames, rep(exec, 2)...)  // the Sees probe, then the execute tap
	frames = append(frames, rep(claim, 2)...) // post-tap CurrentScreen, then Sees: gone
	frames = append(frames, rep(claim, 2)...) // the poll, then the second claim
	frames = append(frames, celebrating)
	rt, tr := ctxFor(t, frames...)

	fn, _ := Get("radar")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("radar: %v", err)
	}
	if taps := countTaps(tr); taps != 6 {
		t.Errorf("got %d taps, want 6 (navigation, banked claim + dismiss, execute, claim + dismiss)", taps)
	}
}

// No targets: nothing to do, and that is success.
func TestRadarSucceedsWithNoTargets(t *testing.T) {
	_, _, _, done := radarStates()

	rt, tr := ctxFor(t, append(toRadar(), rep(done, 4)...)...)
	fn, _ := Get("radar")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("no targets must be success: %v", err)
	}
	if taps := countTaps(tr); taps != 1 {
		t.Errorf("got %d taps, want only the navigation tap", taps)
	}
}

// Quick Execute renders enabled when stamina is short, and tapping it opens a
// purchase dialog. Leaving is the entire correct interaction, and it is an
// ordinary outcome — stamina regenerates before the next run.
func TestRadarBacksOutOfTheStaminaPromptWithoutTappingAnything(t *testing.T) {
	exec, _, _, done := radarStates()
	prompt := runtimetest.Frame(vision.ScreenStaminaPrompt)

	frames := toRadar()
	// WaitFor, arrival, the banked check, the Sees probe, the execute tap.
	frames = append(frames, rep(exec, 5)...)
	// The post-tap CurrentScreen sees the dialog, then NavigateTo does.
	frames = append(frames, rep(prompt, 3)...)
	frames = append(frames, rep(done, 3)...)
	rt, tr := ctxFor(t, frames...)

	fn, _ := Get("radar")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("insufficient stamina is an ordinary outcome, not a failure: %v", err)
	}
	// The navigation tap and Quick Execute, and nothing else: the dialog
	// spends currency, so nothing inside it may be tapped. It is left by the
	// graph's back edge, which is why a back press is the proof.
	if taps := countTaps(tr); taps != 2 {
		t.Errorf("got %d taps, want exactly 2 — nothing in the stamina dialog may be tapped", taps)
	}
	if backs := countBacks(tr); backs == 0 {
		t.Error("want a back press to leave the stamina dialog")
	}
}

// Once an execution really is underway the claim can still take its time, and
// the budget is wall clock so it can wait. The retired bound was eight
// *attempts*, which sounds generous and was worth only 75-109s on this handset
// because every attempt costs a screencap plus a full recognition pass.
//
// The wait now begins only after the button has gone, so the polled state is
// `done` — executing, nothing claimable yet — rather than a screen still
// showing Quick Execute. That combination is no longer reachable: it means the
// tap was ignored, which is ErrExecuteIgnored.
func TestRadarPollsPatientlyForALateClaim(t *testing.T) {
	exec, claim, celebrating, done := radarStates()

	frames := toRadar()
	// WaitFor, arrival, the banked check, the Sees probe, the execute tap.
	frames = append(frames, rep(exec, 5)...)
	// The post-tap CurrentScreen and the Sees probe that finds it gone.
	frames = append(frames, rep(done, 2)...)
	// Twenty fruitless poll iterations at two frames each.
	frames = append(frames, rep(done, 40)...)
	// The poll's CurrentScreen, then the claim tap.
	frames = append(frames, rep(claim, 2)...)
	frames = append(frames, celebrating)
	frames = append(frames, rep(done, 2)...)
	rt, tr := ctxFor(t, frames...)

	fn, ok := Get("radar")
	if !ok {
		t.Fatal("radar not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("radar: %v", err)
	}
	if taps := countTaps(tr); taps != 4 {
		t.Errorf("got %d taps, want 4 (navigation, execute, claim, dismiss)", taps)
	}
}

// Claim All is revealed after an execution and the screen never changes —
// both states are `radar` — so WaitFor cannot express this wait and only the
// anchor's arrival can. If it never arrives within the budget, that is a
// fault, not a quiet success.
func TestRadarFailsWhenClaimAllNeverAppears(t *testing.T) {
	exec, _, _, done := radarStates()

	// Collapse the budget rather than counting frames: the loop is bounded by
	// wall clock now, so the number of polls it fits is not a fixed number.
	restore := claimPollBudget
	claimPollBudget = 20 * time.Millisecond
	t.Cleanup(func() { claimPollBudget = restore })

	// The execution must genuinely start first, or this would be testing
	// ErrExecuteIgnored instead. ReplayTransport then holds `done`, so the
	// claim simply never arrives.
	frames := append(toRadar(), rep(exec, 5)...)
	frames = append(frames, rep(done, 4)...)
	rt, _ := ctxFor(t, frames...)
	fn, _ := Get("radar")
	if err := fn(context.Background(), rt); !errors.Is(err, ErrClaimNeverAppeared) {
		t.Fatalf("got %v, want ErrClaimNeverAppeared", err)
	}
}

// A tap that the game swallows leaves Quick Execute exactly where it was, and
// the run then waits out the whole claim budget for the reward of an execution
// that never started. Run 356 did this for 260s while stamina went 46 -> 47 —
// regeneration, not the 10 an execution costs.
//
// So the tap is retried until the button goes away, which is the only local
// proof the game accepted it.
func TestRadarRetapsQuickExecuteWhenTheGameIgnoresIt(t *testing.T) {
	exec, claim, celebrating, done := radarStates()

	frames := toRadar()
	// WaitFor, arrival, the banked check, then the first Sees probe and the
	// first execute tap, then the post-tap CurrentScreen. Six so far.
	frames = append(frames, rep(exec, 6)...)
	// The second Sees probe still finds Quick Execute — the game swallowed
	// the tap — so it is tapped again. Eight.
	frames = append(frames, rep(exec, 2)...)
	// The post-tap CurrentScreen, then the Sees probe that finds it gone.
	frames = append(frames, rep(claim, 2)...)
	// The claim poll's CurrentScreen, then the claim tap.
	frames = append(frames, rep(claim, 2)...)
	frames = append(frames, celebrating)
	frames = append(frames, rep(done, 2)...)
	rt, tr := ctxFor(t, frames...)

	fn, _ := Get("radar")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("radar: %v", err)
	}
	// Navigation, two execute taps, the claim, the dismiss.
	if taps := countTaps(tr); taps != 5 {
		t.Errorf("got %d taps, want 5 (navigation, execute x2, claim, dismiss)", taps)
	}
}

// A tap the game never accepts must fail fast and name the execute, not the
// claim. Blaming the claim is what sent three separate investigations at the
// wrong half of the task.
func TestRadarFailsFastWhenTheExecuteTapIsNeverAccepted(t *testing.T) {
	exec, _, _, _ := radarStates()

	// ReplayTransport holds its last frame, so Quick Execute simply never
	// goes away however many times it is tapped.
	frames := append(toRadar(), rep(exec, 6)...)
	rt, _ := ctxFor(t, frames...)

	fn, _ := Get("radar")
	err := fn(context.Background(), rt)
	if !errors.Is(err, ErrExecuteIgnored) {
		t.Fatalf("got %v, want ErrExecuteIgnored", err)
	}
	if errors.Is(err, ErrClaimNeverAppeared) {
		t.Error("blamed the claim for an execute that never started")
	}
}
