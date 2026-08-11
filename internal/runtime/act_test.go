package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

func TestTapHitsInsideMatchedAnchor(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "base")
	if err := c.Tap(context.Background(), "base", "collect_bubble"); err != nil {
		t.Fatal(err)
	}
	acts := tr.Actions()
	if len(acts) != 1 || acts[0].Kind != "tap" {
		t.Fatalf("actions: %+v", acts)
	}
	// The tap must land inside the anchor's own grid cell. Asking runtimetest
	// for the region beats writing the pixels down: cell assignment shifts
	// whenever the synthetic layout gains an anchor, and a hard-coded box
	// would then pass or fail for reasons having nothing to do with Tap.
	r := runtimetest.AnchorRegion("base", "collect_bubble")
	p := acts[0].At
	if p.X < r.X1 || p.X > r.X2 || p.Y < r.Y1 || p.Y > r.Y2 {
		t.Fatalf("tap at %+v outside the matched anchor's region %+v", p, r)
	}
}

func TestTapWrongScreenRefused(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "mail")
	err := c.Tap(context.Background(), "base", "collect_bubble")
	if !errors.Is(err, runtime.ErrWrongScreen) {
		t.Fatalf("got %v, want ErrWrongScreen", err)
	}
	if len(tr.Actions()) != 0 {
		t.Fatalf("tapped despite wrong screen: %+v", tr.Actions())
	}
}

func TestTapUnknownAnchorErrors(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	if err := c.Tap(context.Background(), "base", "no_such_anchor"); err == nil {
		t.Fatal("unknown anchor accepted")
	}
}

func TestSwipeRefusedOnUnrecognizedScreen(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "")
	err := c.Swipe(context.Background(),
		transport.Norm{X: 0.5, Y: 0.8}, transport.Norm{X: 0.5, Y: 0.2})
	if !errors.Is(err, runtime.ErrLost) {
		t.Fatalf("got %v, want ErrLost after failed recovery", err)
	}
	// The panic route may press back/restart, but no swipe may ever land on
	// an unrecognized screen.
	for _, a := range tr.Actions() {
		if a.Kind == "swipe" {
			t.Fatalf("swiped blind: %+v", tr.Actions())
		}
	}
}

func TestSwipeJittersEndpointsAndDuration(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "base")
	from, to := transport.Norm{X: 0.5, Y: 0.8}, transport.Norm{X: 0.5, Y: 0.2}
	if err := c.Swipe(context.Background(), from, to); err != nil {
		t.Fatal(err)
	}
	a := tr.Actions()[0]
	if a.Kind != "swipe" || a.Duration <= 0 {
		t.Fatalf("swipe action: %+v", a)
	}
	if !a.At.Valid() || !a.To.Valid() {
		t.Fatalf("jittered endpoints left the unit square: %+v", a)
	}
	if a.At == from && a.To == to {
		t.Fatal("swipe endpoints not jittered at all")
	}
}

func TestTypeTextRequiresRecognizedScreen(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "base")
	if err := c.TypeText(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if acts := tr.Actions(); len(acts) != 1 || acts[0].Text != "hello" {
		t.Fatalf("actions: %+v", acts)
	}
}

// A task that must confirm an action landed needs to ask whether a control is
// still there, and Tap was the only anchor query available — which means the
// probe was itself an action. That is what left radar unable to tell "the game
// accepted my tap" from "the game ignored it".
func TestSeesReportsAnchorPresenceWithoutTapping(t *testing.T) {
	withButton := runtimetest.FrameWithout(vision.ScreenRadar, "claim_all_button", "rewards_banner")
	withoutButton := runtimetest.FrameWithout(vision.ScreenRadar, "quick_execute_button", "rewards_banner")
	tr, err := transport.NewReplayTransportFromImages(withButton, withoutButton)
	if err != nil {
		t.Fatal(err)
	}
	c, err := runtime.New(runtimetest.Options(tr, &runtimetest.FakeKill{}))
	if err != nil {
		t.Fatal(err)
	}

	seen, err := c.Sees(context.Background(), vision.ScreenRadar, "quick_execute_button")
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Error("Sees reported the button absent while it was on screen")
	}

	seen, err = c.Sees(context.Background(), vision.ScreenRadar, "quick_execute_button")
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Error("Sees reported the button present after it went away")
	}

	if n := len(tr.Actions()); n != 0 {
		t.Errorf("Sees performed %d actions, want 0 — it is a query, not an act: %+v", n, tr.Actions())
	}
}
