package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
)

func TestTapHitsInsideMatchedAnchor(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "base")
	if err := c.Tap(context.Background(), "base", "gather_button"); err != nil {
		t.Fatal(err)
	}
	acts := tr.Actions()
	if len(acts) != 1 || acts[0].Kind != "tap" {
		t.Fatalf("actions: %+v", acts)
	}
	// The synthetic base pattern occupies pixels (4,4)-(12,12) of 64 → the
	// tap must land inside that box in normalized coordinates.
	p := acts[0].At
	if p.X < 4.0/64 || p.X > 12.0/64 || p.Y < 4.0/64 || p.Y > 12.0/64 {
		t.Fatalf("tap at %+v outside the matched anchor box", p)
	}
}

func TestTapWrongScreenRefused(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "mail")
	err := c.Tap(context.Background(), "base", "gather_button")
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
