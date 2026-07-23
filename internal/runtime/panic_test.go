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

func TestPanicRouteRespectsKillSwitch(t *testing.T) {
	ks := &runtimetest.FakeKill{}
	c, _ := newCtx(t, ks, "", "", "", "", "base")
	ks.Set(runtime.ErrPaused)
	_, err := c.CurrentScreen(context.Background())
	if !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("got %v, want ErrPaused", err)
	}
}
