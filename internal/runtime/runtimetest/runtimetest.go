// Package runtimetest provides synthetic screens, a matching graph, and
// fakes for testing the task runtime with no device attached.
//
// Five screens share one 8x8 template, distinguished by position: each
// screen's anchors search only its own region of the 64x64 frame, and
// Frame(screen) renders the pattern only at that screen's spot. Recognition
// is therefore exact and deterministic without needing patterns that are
// provably NCC-distinct from each other.
package runtimetest

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"sync"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// spot places one screen's pattern and the region its anchors search.
type spot struct {
	px, py int            // paste position of the 8x8 pattern
	region transport.Rect // search region containing it, disjoint from others
}

var spots = map[string]spot{
	"base":          {4, 4, transport.Rect{X1: 0, Y1: 0, X2: 0.45, Y2: 0.45}},
	"alliance":      {44, 4, transport.Rect{X1: 0.55, Y1: 0, X2: 1, Y2: 0.45}},
	"mail":          {4, 44, transport.Rect{X1: 0, Y1: 0.55, X2: 0.45, Y2: 1}},
	"radar":         {44, 44, transport.Rect{X1: 0.55, Y1: 0.55, X2: 1, Y2: 1}},
	"alliance_tech": {28, 28, transport.Rect{X1: 0.42, Y1: 0.42, X2: 0.58, Y2: 0.58}},
}

// tapAnchors names the tap targets each Tier 1 skeleton uses, per screen.
// The radar screen carries two because its task split into radar_quick and
// radar_claim, each with its own button.
var tapAnchors = map[string][]string{
	"base":          {"gather_button"},
	"alliance":      {"help_all_button"},
	"mail":          {"collect_all_button"},
	"alliance_tech": {"donate_button"},
	"radar":         {"radar_quick_button", "radar_claim_button"},
}

func pattern() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			g.SetGray(x, y, color.Gray{Y: uint8((x*37 + y*53) % 256)})
		}
	}
	return g
}

// Registry builds the five-screen synthetic registry at ReferenceHeight 64.
// Each screen has an identifying anchor plus the tap anchor its skeleton
// task needs; both share the screen's pattern spot.
func Registry() *vision.Registry {
	p := pattern()
	reg := &vision.Registry{ReferenceHeight: 64}
	for _, name := range []string{"alliance", "alliance_tech", "base", "mail", "radar"} {
		s := spots[name]
		anchors := []vision.Anchor{
			{ID: "id", Template: p, Region: s.region, Threshold: 0.9, IdentifiesScreen: true},
		}
		for _, btn := range tapAnchors[name] {
			anchors = append(anchors, vision.Anchor{ID: btn, Template: p, Region: s.region, Threshold: 0.9})
		}
		reg.Screens = append(reg.Screens, vision.Screen{Name: name, Anchors: anchors})
	}
	return reg
}

// Graph mirrors DefaultGraph's topology over the synthetic screens, using
// the "id" anchor as every tap edge's target (it is present and matchable
// on each screen's frame).
func Graph() *runtime.Graph {
	return &runtime.Graph{
		Entry: "base",
		Edges: []runtime.Edge{
			{From: "base", To: "alliance", Action: runtime.ActionTap, AnchorID: "id"},
			{From: "alliance", To: "base", Action: runtime.ActionBack},
			{From: "alliance", To: "alliance_tech", Action: runtime.ActionTap, AnchorID: "id"},
			{From: "alliance_tech", To: "alliance", Action: runtime.ActionBack},
			{From: "base", To: "mail", Action: runtime.ActionTap, AnchorID: "id"},
			{From: "mail", To: "base", Action: runtime.ActionBack},
			{From: "base", To: "radar", Action: runtime.ActionTap, AnchorID: "id"},
			{From: "radar", To: "base", Action: runtime.ActionBack},
		},
	}
}

// Frame renders the named screen: flat gray with the pattern at the
// screen's spot. Panics on unknown names — a test bug, not a runtime case.
func Frame(screen string) image.Image {
	s, ok := spots[screen]
	if !ok {
		panic(fmt.Sprintf("runtimetest: unknown screen %q", screen))
	}
	out := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			out.SetGray(x, y, color.Gray{Y: 100})
		}
	}
	p := pattern()
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			out.SetGray(s.px+x, s.py+y, p.GrayAt(x, y))
		}
	}
	return out
}

// Blank is a frame no screen recognizes.
func Blank() image.Image {
	out := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			out.SetGray(x, y, color.Gray{Y: 100})
		}
	}
	return out
}

// Frames is a convenience for staging a replay script by screen name;
// "" stages a Blank frame.
func Frames(names ...string) []image.Image {
	out := make([]image.Image, len(names))
	for i, n := range names {
		if n == "" {
			out[i] = Blank()
		} else {
			out[i] = Frame(n)
		}
	}
	return out
}

// FakeKill is a settable kill switch for tests.
type FakeKill struct {
	mu  sync.Mutex
	err error
}

// Set makes every subsequent Check return err (nil re-enables).
func (k *FakeKill) Set(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

func (k *FakeKill) Check(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

// Options returns runtime.Options wired for fast deterministic tests:
// synthetic registry and graph, seeded rand, millisecond timing.
func Options(tr transport.Transport, ks runtime.KillSwitch) runtime.Options {
	return runtime.Options{
		Transport:      tr,
		Registry:       Registry(),
		Graph:          Graph(),
		Kill:           ks,
		AccountID:      1,
		Rand:           rand.New(rand.NewSource(42)),
		PollInterval:   time.Millisecond,
		WaitTimeout:    50 * time.Millisecond,
		RestartTimeout: 50 * time.Millisecond,
	}
}
