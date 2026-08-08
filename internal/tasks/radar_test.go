package tasks

import (
	"context"
	"errors"
	"image"
	"testing"

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
	// WaitFor, arrival CurrentScreen, the banked-claim check, the execute tap.
	frames = append(frames, rep(exec, 4)...)
	// The poll's CurrentScreen, then the claim tap.
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
	frames = append(frames, rep(exec, 1)...)  // now Quick Execute is back
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
	frames = append(frames, rep(exec, 4)...)
	frames = append(frames, rep(prompt, 3)...) // the poll sees it, then NavigateTo does
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

// Claim All is revealed a few seconds after an execution and the screen
// never changes — both states are `radar` — so WaitFor cannot express this
// wait and only the anchor's arrival can. If it never arrives, that is a
// fault, not a quiet success.
func TestRadarFailsWhenClaimAllNeverAppears(t *testing.T) {
	exec, _, _, _ := radarStates()

	frames := append(toRadar(), rep(exec, 4*claimPollAttempts)...)
	rt, _ := ctxFor(t, frames...)
	fn, _ := Get("radar")
	if err := fn(context.Background(), rt); !errors.Is(err, ErrClaimNeverAppeared) {
		t.Fatalf("got %v, want ErrClaimNeverAppeared", err)
	}
}
