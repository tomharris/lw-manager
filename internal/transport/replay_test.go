package transport

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"
	"time"
)

// solidFrame makes a distinguishable frame: every pixel carries `tag`, so a
// test can tell which frame it was served.
func solidFrame(w, h int, tag uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: tag, G: tag, B: tag, A: 255})
		}
	}
	return img
}

func tagOf(t *testing.T, img image.Image) uint8 {
	t.Helper()
	r, _, _, _ := img.At(0, 0).RGBA()
	return uint8(r >> 8)
}

func newReplay(t *testing.T, tags ...uint8) *ReplayTransport {
	t.Helper()
	imgs := make([]image.Image, len(tags))
	for i, tag := range tags {
		imgs[i] = solidFrame(100, 200, tag)
	}
	rt, err := NewReplayTransportFromImages(imgs...)
	if err != nil {
		t.Fatalf("NewReplayTransportFromImages(): %v", err)
	}
	return rt
}

func TestReplayServesFramesInOrder(t *testing.T) {
	ctx := context.Background()
	rt := newReplay(t, 1, 2, 3)

	for _, want := range []uint8{1, 2, 3} {
		img, err := rt.Screenshot(ctx)
		if err != nil {
			t.Fatalf("Screenshot(): %v", err)
		}
		if got := tagOf(t, img); got != want {
			t.Errorf("served frame %d, want %d", got, want)
		}
	}
}

// Hold-last-frame: a poll-until-recognized loop settles on a stable screen
// the way it would against a real idle device.
func TestReplayHoldsLastFrame(t *testing.T) {
	ctx := context.Background()
	rt := newReplay(t, 1, 2)

	for i := 0; i < 10; i++ {
		img, err := rt.Screenshot(ctx)
		if err != nil {
			t.Fatalf("Screenshot() #%d: %v", i, err)
		}
		want := uint8(2)
		if i == 0 {
			want = 1
		}
		if got := tagOf(t, img); got != want {
			t.Errorf("call %d served frame %d, want %d", i, got, want)
		}
	}
}

// The cap is what stops a non-converging task from hanging the suite.
func TestReplayCapsTotalServes(t *testing.T) {
	ctx := context.Background()
	rt := newReplay(t, 1)
	rt.MaxServes = 5

	for i := 0; i < 5; i++ {
		if _, err := rt.Screenshot(ctx); err != nil {
			t.Fatalf("Screenshot() #%d within budget: %v", i, err)
		}
	}

	_, err := rt.Screenshot(ctx)
	if !errors.Is(err, ErrFramesExhausted) {
		t.Fatalf("error past the cap = %v, want ErrFramesExhausted", err)
	}
}

func TestReplayDefaultCapApplies(t *testing.T) {
	ctx := context.Background()
	rt := newReplay(t, 1)

	for i := 0; i < DefaultMaxServes; i++ {
		if _, err := rt.Screenshot(ctx); err != nil {
			t.Fatalf("Screenshot() #%d within default budget: %v", i, err)
		}
	}
	if _, err := rt.Screenshot(ctx); !errors.Is(err, ErrFramesExhausted) {
		t.Fatalf("error past DefaultMaxServes = %v, want ErrFramesExhausted", err)
	}
}

func TestReplayResetRestoresBudget(t *testing.T) {
	ctx := context.Background()
	rt := newReplay(t, 1, 2)
	rt.MaxServes = 3

	for i := 0; i < 3; i++ {
		if _, err := rt.Screenshot(ctx); err != nil {
			t.Fatalf("Screenshot() #%d: %v", i, err)
		}
	}
	rt.Reset()

	img, err := rt.Screenshot(ctx)
	if err != nil {
		t.Fatalf("Screenshot() after Reset(): %v", err)
	}
	if got := tagOf(t, img); got != 1 {
		t.Errorf("after Reset() served frame %d, want the first frame", got)
	}
}

func TestReplayRecordsActions(t *testing.T) {
	ctx := context.Background()
	rt := newReplay(t, 1)

	if err := rt.Tap(ctx, Norm{X: 0.5, Y: 0.5}); err != nil {
		t.Fatalf("Tap(): %v", err)
	}
	if err := rt.Swipe(ctx, Norm{X: 0.5, Y: 0.8}, Norm{X: 0.5, Y: 0.2}, 300*time.Millisecond); err != nil {
		t.Fatalf("Swipe(): %v", err)
	}
	if err := rt.AppRestart(ctx); err != nil {
		t.Fatalf("AppRestart(): %v", err)
	}

	got := rt.Actions()
	if len(got) != 3 {
		t.Fatalf("recorded %d actions, want 3", len(got))
	}
	for i, want := range []string{"tap", "swipe", "restart"} {
		if got[i].Kind != want {
			t.Errorf("action %d kind = %q, want %q", i, got[i].Kind, want)
		}
	}
	if got[1].Duration != 300*time.Millisecond {
		t.Errorf("swipe duration = %v, want 300ms", got[1].Duration)
	}
}

// Out-of-range coordinates are a caller bug and must not reach a device,
// real or fake.
func TestReplayRejectsOutOfRangeTaps(t *testing.T) {
	rt := newReplay(t, 1)
	err := rt.Tap(context.Background(), Norm{X: 1.5, Y: 0.5})

	var oor ErrOutOfRange
	if !errors.As(err, &oor) {
		t.Fatalf("Tap() with x=1.5 returned %v, want ErrOutOfRange", err)
	}
	if len(rt.Actions()) != 0 {
		t.Error("a rejected tap was still recorded")
	}
}

func TestReplayResolutionFromFirstFrame(t *testing.T) {
	rt := newReplay(t, 1)
	if got := rt.Resolution(); got.X != 100 || got.Y != 200 {
		t.Errorf("Resolution() = %v, want (100, 200)", got)
	}
}

// ReplayTransport must satisfy Transport, or it cannot stand in for a device.
var _ Transport = (*ReplayTransport)(nil)
var _ Transport = (*ADBTransport)(nil)

func TestReplayRecordsBack(t *testing.T) {
	rt, err := NewReplayTransportFromImages(image.NewGray(image.Rect(0, 0, 4, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Back(context.Background()); err != nil {
		t.Fatalf("Back: %v", err)
	}
	acts := rt.Actions()
	if len(acts) != 1 || acts[0].Kind != "back" {
		t.Fatalf("actions: got %+v, want one back", acts)
	}
	// The interface must carry Back, or the panic route cannot use it.
	var _ Transport = rt
}
