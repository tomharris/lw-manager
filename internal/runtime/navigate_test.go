package runtime_test

import (
	"context"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
)

func TestNavigateToMultiHop(t *testing.T) {
	// base → alliance → alliance_tech. Frame schedule (one frame per
	// Screenshot call): CurrentScreen, hop1 tap verify, hop1 WaitFor, hop2
	// tap verify, hop2 WaitFor, final CurrentScreen; the replay holds the
	// last frame for any extra polls.
	c, _ := newCtx(t, &runtimetest.FakeKill{},
		"base", "base", "alliance", "alliance", "alliance_tech", "alliance_tech")
	if err := c.NavigateTo(context.Background(), "alliance_tech"); err != nil {
		t.Fatal(err)
	}
}

func TestNavigateToAlreadyThere(t *testing.T) {
	c, tr := newCtx(t, &runtimetest.FakeKill{}, "mail")
	if err := c.NavigateTo(context.Background(), "mail"); err != nil {
		t.Fatal(err)
	}
	if len(tr.Actions()) != 0 {
		t.Fatalf("acted while already on target: %+v", tr.Actions())
	}
}

func TestNavigateToReplansAfterUnexpectedScreen(t *testing.T) {
	// Heading base → alliance, but the tap "lands" on mail (a popup ate the
	// tap, say). WaitFor(alliance) sees mail 5× (wrong-screen streak), then
	// NavigateTo re-plans from mail: back to base, tap to alliance.
	c, tr := newCtx(t, &runtimetest.FakeKill{},
		"base",                                 // CurrentScreen
		"base",                                 // hop tap verify
		"mail", "mail", "mail", "mail", "mail", // WaitFor streak
		"mail",     // re-plan CurrentScreen
		"base",     // back-hop WaitFor
		"base",     // tap verify
		"alliance", // WaitFor
		"alliance") // final CurrentScreen
	if err := c.NavigateTo(context.Background(), "alliance"); err != nil {
		t.Fatal(err)
	}
	acts := tr.Actions()
	if countKind(acts, "back") != 1 {
		t.Fatalf("back presses: got %d, want 1 (the re-planned mail→base hop)", countKind(acts, "back"))
	}
	if countKind(acts, "tap") != 2 {
		t.Fatalf("taps: got %d, want 2", countKind(acts, "tap"))
	}
}

func TestNavigateToUnknownScreenFails(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "alliance_tech")
	if err := c.NavigateTo(context.Background(), "no_such_screen"); err == nil {
		t.Fatal("navigated to a screen the graph does not know")
	}
}
