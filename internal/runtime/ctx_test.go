package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

func newCtx(t *testing.T, ks runtime.KillSwitch, frames ...string) (*runtime.Ctx, *transport.ReplayTransport) {
	t.Helper()
	tr, err := transport.NewReplayTransportFromImages(runtimetest.Frames(frames...)...)
	if err != nil {
		t.Fatal(err)
	}
	c, err := runtime.New(runtimetest.Options(tr, ks))
	if err != nil {
		t.Fatal(err)
	}
	return c, tr
}

func TestNewValidatesGraphAgainstRegistry(t *testing.T) {
	tr, _ := transport.NewReplayTransportFromImages(runtimetest.Blank())
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Graph = &runtime.Graph{Entry: "nonexistent"}
	if _, err := runtime.New(opts); err == nil {
		t.Fatal("New accepted a graph naming an unknown screen")
	}
}

func TestCurrentScreenRecognizes(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "alliance")
	r, err := c.CurrentScreen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Screen != "alliance" || r.Confidence < 0.9 {
		t.Fatalf("recognition: got %+v", r)
	}
}

func TestWaitForPollsUntilScreenAppears(t *testing.T) {
	// Two unrecognizable frames, then mail: WaitFor must poll through them.
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "", "", "mail")
	r, err := c.WaitFor(context.Background(), "mail")
	if err != nil {
		t.Fatal(err)
	}
	if r.Screen != "mail" {
		t.Fatalf("screen: got %q", r.Screen)
	}
}

func TestWaitForGivesUpOnSteadyWrongScreen(t *testing.T) {
	// The device sits on base forever; waiting for mail must fail with
	// ErrWrongScreen after the wrong-screen streak, not hang to timeout.
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	_, err := c.WaitFor(context.Background(), "mail")
	if !errors.Is(err, runtime.ErrWrongScreen) {
		t.Fatalf("got %v, want ErrWrongScreen", err)
	}
}

func TestPrimitivesCheckKillSwitchFirst(t *testing.T) {
	ks := &runtimetest.FakeKill{}
	ks.Set(runtime.ErrPaused)
	c, tr := newCtx(t, ks, "base")

	if _, err := c.CurrentScreen(context.Background()); !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("CurrentScreen: got %v, want ErrPaused", err)
	}
	if err := c.Sleep(context.Background(), time.Millisecond, 2*time.Millisecond); !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("Sleep: got %v, want ErrPaused", err)
	}
	if len(tr.Actions()) != 0 {
		t.Fatalf("paused ctx touched the device: %+v", tr.Actions())
	}
}

func TestSleepRespectsContextCancel(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour, 2*time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestUnrecognizedScreenSurfacesLost(t *testing.T) {
	// A permanently unrecognized screen must surface ErrLost after failed
	// recovery — never vision.ErrNoScreenRecognized, which task code is not
	// supposed to see.
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "")
	_, err := c.CurrentScreen(context.Background())
	if !errors.Is(err, runtime.ErrLost) {
		t.Fatalf("got %v, want ErrLost", err)
	}
	if errors.Is(err, vision.ErrNoScreenRecognized) {
		t.Fatal("vision sentinel leaked through the runtime boundary")
	}
}
