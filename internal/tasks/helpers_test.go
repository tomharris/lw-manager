package tasks

import (
	"context"
	"errors"
	"image"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ctxFor stages a replay over the given frames and wires a Ctx to it. The
// replay holds its last frame once the script runs out, so a script needs to
// be exact only up to the last frame that must differ from its predecessor.
func ctxFor(t *testing.T, frames ...image.Image) (*runtime.Ctx, *transport.ReplayTransport) {
	t.Helper()
	tr, err := transport.NewReplayTransportFromImages(frames...)
	if err != nil {
		t.Fatal(err)
	}
	c, err := runtime.New(runtimetest.Options(tr, &runtimetest.FakeKill{}))
	if err != nil {
		t.Fatal(err)
	}
	return c, tr
}

// rep stages n copies of one frame, so a script can say how long the device
// sits on a state instead of repeating the constructor.
func rep(img image.Image, n int) []image.Image {
	out := make([]image.Image, n)
	for i := range out {
		out[i] = img
	}
	return out
}

func countKind(tr *transport.ReplayTransport, kind string) int {
	n := 0
	for _, a := range tr.Actions() {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

func countTaps(tr *transport.ReplayTransport) int  { return countKind(tr, "tap") }
func countBacks(tr *transport.ReplayTransport) int { return countKind(tr, "back") }

func TestTapIfPresentReportsTrueWhenTheAnchorIsThere(t *testing.T) {
	rt, tr := ctxFor(t, runtimetest.Frames(vision.ScreenBase, vision.ScreenBase)...)
	ok, err := tapIfPresent(context.Background(), rt, vision.ScreenBase, "collect_bubble")
	if err != nil {
		t.Fatalf("tapIfPresent: %v", err)
	}
	if !ok {
		t.Error("want ok=true when the anchor is rendered")
	}
	if taps := countTaps(tr); taps != 1 {
		t.Errorf("got %d taps, want 1", taps)
	}
}

// A missing anchor is an ordinary outcome, not an error: no bubble means
// nothing accumulated, and no help icon means nobody needs help.
func TestTapIfPresentReportsFalseWithoutTappingWhenAbsent(t *testing.T) {
	f := runtimetest.FrameWithout(vision.ScreenBase, "collect_bubble")
	rt, tr := ctxFor(t, f, f)
	ok, err := tapIfPresent(context.Background(), rt, vision.ScreenBase, "collect_bubble")
	if err != nil {
		t.Fatalf("tapIfPresent must not error on a missing anchor: %v", err)
	}
	if ok {
		t.Error("want ok=false when the anchor is not rendered")
	}
	if taps := countTaps(tr); taps != 0 {
		t.Errorf("got %d taps, want 0 — a missing anchor must not be tapped", taps)
	}
}

// Being on the wrong screen is not the same as the button being absent.
// Conflating them is how a task reports success for doing nothing.
func TestTapIfPresentPropagatesWrongScreen(t *testing.T) {
	f := runtimetest.Frame(vision.ScreenMail)
	rt, tr := ctxFor(t, f, f)
	ok, err := tapIfPresent(context.Background(), rt, vision.ScreenBase, "collect_bubble")
	if !errors.Is(err, runtime.ErrWrongScreen) {
		t.Fatalf("got %v, want ErrWrongScreen", err)
	}
	if ok {
		t.Error("want ok=false alongside the error")
	}
	if taps := countTaps(tr); taps != 0 {
		t.Errorf("got %d taps, want 0", taps)
	}
}

func TestDismissRewardsTapsTheBannerWhenItIsUp(t *testing.T) {
	f := runtimetest.FrameCelebrating(vision.ScreenMailAlliance)
	rt, tr := ctxFor(t, f, f, f)
	if err := dismissRewards(context.Background(), rt, vision.ScreenMailAlliance); err != nil {
		t.Fatalf("dismissRewards: %v", err)
	}
	if taps := countTaps(tr); taps != 1 {
		t.Errorf("got %d taps, want 1", taps)
	}
}

// The banner is self-gating: no banner means no celebration means no tap at
// all. This is the property that makes the helper safe — the rejected
// alternative, tapping the bottom-left back arrow, would navigate away here.
func TestDismissRewardsTapsNothingWhenNoBannerIsUp(t *testing.T) {
	f := runtimetest.FrameWithout(vision.ScreenMailAlliance, "rewards_banner")
	rt, tr := ctxFor(t, f, f, f)
	if err := dismissRewards(context.Background(), rt, vision.ScreenMailAlliance); err != nil {
		t.Fatalf("a missing banner must not be an error: %v", err)
	}
	if taps := countTaps(tr); taps != 0 {
		t.Errorf("got %d taps, want 0 — nothing to dismiss must mean nothing tapped", taps)
	}
	if backs := countBacks(tr); backs != 0 {
		t.Errorf("got %d back presses, want 0 — the panic route must not fire", backs)
	}
}

func TestTapUntilGoneStopsWhenTheAnchorDisappears(t *testing.T) {
	present := runtimetest.Frame(vision.ScreenAllianceTechDonate)
	absent := runtimetest.FrameWithout(vision.ScreenAllianceTechDonate, "donate_button")
	rt, tr := ctxFor(t, present, present, present, present, absent, absent)
	n, err := tapUntilGone(context.Background(), rt, vision.ScreenAllianceTechDonate, "donate_button", 10)
	if err != nil {
		t.Fatalf("tapUntilGone: %v", err)
	}
	if n == 0 {
		t.Error("want at least one tap before the anchor vanished")
	}
	if taps := countTaps(tr); taps != n {
		t.Errorf("reported %d taps but transport saw %d", n, taps)
	}
}

// The cap is the backstop against a greyed-out button that never stops
// matching. Reaching it means the discriminator has failed, which is a bug
// and must be loud — a silent success here is exactly the failure mode this
// milestone keeps finding.
func TestTapUntilGoneErrorsWhenItReachesTheCap(t *testing.T) {
	f := runtimetest.Frame(vision.ScreenAllianceTechDonate)
	rt, _ := ctxFor(t, rep(f, 12)...)
	n, err := tapUntilGone(context.Background(), rt, vision.ScreenAllianceTechDonate, "donate_button", 3)
	if !errors.Is(err, ErrTapCapReached) {
		t.Fatalf("got %v, want ErrTapCapReached", err)
	}
	if n != 3 {
		t.Errorf("got %d taps, want the cap of 3", n)
	}
}
