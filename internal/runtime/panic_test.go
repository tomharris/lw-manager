package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
)

func countKind(acts []transport.Action, kind string) int {
	n := 0
	for _, a := range acts {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

func TestPanicRouteRecoversWithBack(t *testing.T) {
	// Frame 1 unrecognized triggers the route; frame 2 (after one back) is
	// recognizable. CurrentScreen must succeed and report it.
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "", "base")
	r, err := c.CurrentScreen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Screen != "base" {
		t.Fatalf("recovered onto %q, want base", r.Screen)
	}
	acts := tr.Actions()
	if countKind(acts, "back") != 1 {
		t.Fatalf("back presses: got %d, want 1 (%+v)", countKind(acts, "back"), acts)
	}
	if countKind(acts, "restart") != 0 {
		t.Fatal("restarted when back sufficed")
	}
}

func TestPanicRouteFallsBackToRestart(t *testing.T) {
	// Four unrecognized frames absorb the trigger plus all three back
	// attempts; the next frame is the entry screen, reachable only through
	// restart.
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "", "", "", "", "base")
	r, err := c.CurrentScreen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Screen != "base" {
		t.Fatalf("recovered onto %q, want the entry screen", r.Screen)
	}
	acts := tr.Actions()
	if countKind(acts, "back") != 3 {
		t.Fatalf("back presses: got %d, want 3", countKind(acts, "back"))
	}
	if countKind(acts, "restart") != 1 {
		t.Fatalf("restarts: got %d, want 1", countKind(acts, "restart"))
	}
}

func TestPanicRouteGivesUpLost(t *testing.T) {
	// Nothing ever becomes recognizable: the route must end in ErrLost
	// rather than hanging (RestartTimeout is 50ms in runtimetest.Options).
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "")
	_, err := c.CurrentScreen(context.Background())
	if !errors.Is(err, runtime.ErrLost) {
		t.Fatalf("got %v, want ErrLost", err)
	}
	if countKind(tr.Actions(), "restart") != 1 {
		t.Fatal("gave up without trying a restart")
	}
}

// newLostCtx builds a Ctx whose frames never become recognizable, with a
// capturer attached, so the route is forced all the way to ErrLost.
func newLostCtx(t *testing.T, fc *fakeCapturer) *runtime.Ctx {
	t.Helper()
	tr, err := transport.NewReplayTransportFromImages(runtimetest.Frames("")...)
	if err != nil {
		t.Fatal(err)
	}
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Capture = fc
	c, err := runtime.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The panic route is the one place that knows recognition has failed on a
// real device, and it was throwing that frame away. The M2 24-hour run turned
// on this: ten incidents over four hours, every rung of the ladder failing,
// and the only surviving evidence was the error string — which cannot
// distinguish a maintenance banner from an OS dialog from a locked screen.
func TestPanicRouteBlobsTheFrameItGaveUpOn(t *testing.T) {
	fc := &fakeCapturer{}
	c := newLostCtx(t, fc)

	if _, err := c.CurrentScreen(context.Background()); !errors.Is(err, runtime.ErrLost) {
		t.Fatalf("got %v, want ErrLost", err)
	}
	if len(fc.calls) != 1 {
		t.Fatalf("recorded %d frames, want exactly 1 — the frame it gave up on", len(fc.calls))
	}
	got := fc.calls[0]
	if got.screenID == nil {
		t.Fatal("recorded with a nil screen id; want a searchable sentinel, or these frames cannot be found later")
	}
	if *got.screenID != runtime.LostScreenID {
		t.Errorf("screen id %q, want %q", *got.screenID, runtime.LostScreenID)
	}
}

// Evidence is a side effect, never a substitute for the diagnosis. A blob
// store outage during a recognition outage must not convert ErrLost — which
// the runner records and the scheduler backs off on — into something else.
func TestPanicRouteStillReportsLostWhenTheEvidenceCaptureFails(t *testing.T) {
	fc := &fakeCapturer{err: errors.New("blob store down")}
	c := newLostCtx(t, fc)

	if _, err := c.CurrentScreen(context.Background()); !errors.Is(err, runtime.ErrLost) {
		t.Fatalf("got %v, want ErrLost — a failed capture must not mask the real failure", err)
	}
}

// A recovered route has its screen and needs no evidence; capturing there
// would blob a frame on every transient popup the device throws.
func TestPanicRouteDoesNotBlobWhenItRecovers(t *testing.T) {
	fc := &fakeCapturer{}
	tr, err := transport.NewReplayTransportFromImages(runtimetest.Frames("", "base")...)
	if err != nil {
		t.Fatal(err)
	}
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Capture = fc
	c, err := runtime.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.CurrentScreen(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("recorded %d frames on a successful recovery, want 0", len(fc.calls))
	}
}

func TestPanicRouteRespectsKillSwitch(t *testing.T) {
	ks := &runtimetest.FakeKill{}
	c, _ := newCtx(t, ks, "", "", "", "", "base")
	ks.Set(runtime.ErrPaused)
	_, err := c.CurrentScreen(context.Background())
	if !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("got %v, want ErrPaused", err)
	}
}
