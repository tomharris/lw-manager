package tasks_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/tasks"
	"github.com/tomharris/lw-manager/internal/transport"
)

func TestAllTierOneTasksRegistered(t *testing.T) {
	want := []string{"daily_gather", "help_all", "mail_collect", "radar", "tech_donate"}
	if got := tasks.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names: got %v, want %v", got, want)
	}
}

// frame scripts per task: enough recognizable frames to navigate from base
// to the task's screen and perform its tap; the replay holds the last frame
// for trailing polls.
var skeletonScripts = map[string][]string{
	// already on base: CurrentScreen, tap verify (tap-if-present on base itself)
	"daily_gather": {"base", "base"},
	// base → alliance: CurrentScreen, hop verify, WaitFor, final
	// CurrentScreen, then the help tap's verify
	"help_all":     {"base", "base", "alliance", "alliance", "alliance"},
	"mail_collect": {"base", "base", "mail", "mail", "mail"},
	// base → alliance → alliance_tech
	"tech_donate": {"base", "base", "alliance", "alliance", "alliance_tech", "alliance_tech", "alliance_tech"},
	"radar":       {"base", "base", "radar", "radar", "radar"},
}

func TestSkeletonsRunAgainstSyntheticScreens(t *testing.T) {
	for name, script := range skeletonScripts {
		t.Run(name, func(t *testing.T) {
			fn, ok := tasks.Get(name)
			if !ok {
				t.Fatalf("task %q not registered", name)
			}
			tr, err := transport.NewReplayTransportFromImages(runtimetest.Frames(script...)...)
			if err != nil {
				t.Fatal(err)
			}
			c, err := runtime.New(runtimetest.Options(tr, &runtimetest.FakeKill{}))
			if err != nil {
				t.Fatal(err)
			}
			if err := fn(context.Background(), c); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// Every skeleton must have tapped its collect anchor at least once.
			taps := 0
			for _, a := range tr.Actions() {
				if a.Kind == "tap" {
					taps++
				}
			}
			if taps == 0 {
				t.Fatalf("%s performed no taps", name)
			}
		})
	}
}

func TestSkeletonToleratesMissingAnchor(t *testing.T) {
	// help_all when no help button is on screen: the anchor match must
	// fail, and the task must treat that as "nothing to help", not an
	// error. Simulate the missing button by moving the tap anchor's search
	// region to an area every synthetic frame renders flat — the match
	// score there cannot clear the threshold. (Moving the region beats
	// raising the threshold: a pixel-perfect synthetic match can score a
	// legitimate 1.0, which no finite threshold excludes.)
	reg := runtimetest.Registry()
	for si := range reg.Screens {
		if reg.Screens[si].Name != "alliance" {
			continue
		}
		for ai := range reg.Screens[si].Anchors {
			if reg.Screens[si].Anchors[ai].ID == "help_all_button" {
				// Flat gray on alliance frames: no pattern spot lives here.
				reg.Screens[si].Anchors[ai].Region = transport.Rect{X1: 0.05, Y1: 0.6, X2: 0.4, Y2: 0.9}
			}
		}
	}
	fn, _ := tasks.Get("help_all")
	tr, err := transport.NewReplayTransportFromImages(
		runtimetest.Frames("base", "base", "alliance", "alliance", "alliance")...)
	if err != nil {
		t.Fatal(err)
	}
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Registry = reg
	c, err := runtime.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(context.Background(), c); err != nil {
		t.Fatalf("missing anchor must mean 'nothing to do', got: %v", err)
	}
}
