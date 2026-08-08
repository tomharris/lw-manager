package tasks

import (
	"context"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/vision"
)

func TestHelpAllTapsTheHudIconWithoutLeavingBase(t *testing.T) {
	f := runtimetest.Frame(vision.ScreenBase)
	rt, tr := ctxFor(t, f, f, f)
	fn, ok := Get("help_all")
	if !ok {
		t.Fatal("help_all not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("help_all: %v", err)
	}
	if taps := countTaps(tr); taps != 1 {
		t.Errorf("got %d taps, want exactly 1 — help_all must not navigate", taps)
	}
	if backs := countBacks(tr); backs != 0 {
		t.Errorf("got %d back presses, want 0", backs)
	}
}

// No help icon means nobody needs help. That is success, and it is the
// common case at a 180s cadence.
func TestHelpAllSucceedsWhenNobodyNeedsHelp(t *testing.T) {
	f := runtimetest.FrameWithout(vision.ScreenBase, "help_all_button")
	rt, tr := ctxFor(t, f, f, f)
	fn, _ := Get("help_all")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("a missing help icon must be success: %v", err)
	}
	if taps := countTaps(tr); taps != 0 {
		t.Errorf("got %d taps, want 0", taps)
	}
}

// Any one collect bubble gathers everything, so a single tap is the whole
// task — the bubble's anchor is content-addressed with a broad search
// region, and Match returning the best-scoring placement needs no
// disambiguation because any bubble will do.
func TestDailyGatherTapsOneBubble(t *testing.T) {
	f := runtimetest.Frame(vision.ScreenBase)
	rt, tr := ctxFor(t, f, f, f)
	fn, ok := Get("daily_gather")
	if !ok {
		t.Fatal("daily_gather not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("daily_gather: %v", err)
	}
	if taps := countTaps(tr); taps != 1 {
		t.Errorf("got %d taps, want 1", taps)
	}
}

func TestDailyGatherSucceedsWhenNothingHasAccumulated(t *testing.T) {
	f := runtimetest.FrameWithout(vision.ScreenBase, "collect_bubble")
	rt, tr := ctxFor(t, f, f, f)
	fn, _ := Get("daily_gather")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("no bubble must be success: %v", err)
	}
	if taps := countTaps(tr); taps != 0 {
		t.Errorf("got %d taps, want 0", taps)
	}
}
