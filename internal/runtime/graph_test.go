package runtime

import (
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// graphTestRegistry: two screens, each with an identifying anchor and one
// tap anchor, enough to validate edges against.
func graphTestRegistry() *vision.Registry {
	tmpl := image.NewGray(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			tmpl.SetGray(x, y, color.Gray{Y: uint8(40 * (x + y))})
		}
	}
	full := transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}
	anchors := func() []vision.Anchor {
		return []vision.Anchor{
			{ID: "id", Template: tmpl, Region: full, Threshold: 0.9, IdentifiesScreen: true},
			{ID: "go_button", Template: tmpl, Region: full, Threshold: 0.9},
		}
	}
	return &vision.Registry{
		ReferenceHeight: 64,
		Screens: []vision.Screen{
			{Name: "alliance", Anchors: anchors()},
			{Name: "base", Anchors: anchors()},
		},
	}
}

func TestGraphValidate(t *testing.T) {
	reg := graphTestRegistry()
	good := &Graph{Entry: "base", Edges: []Edge{
		{From: "base", To: "alliance", Action: ActionTap, AnchorID: "go_button"},
		{From: "alliance", To: "base", Action: ActionBack},
	}}
	if err := good.Validate(reg); err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}

	cases := []struct {
		name string
		g    *Graph
	}{
		{"unknown entry", &Graph{Entry: "nope", Edges: good.Edges}},
		{"unknown from", &Graph{Entry: "base", Edges: []Edge{{From: "nope", To: "base", Action: ActionBack}}}},
		{"unknown to", &Graph{Entry: "base", Edges: []Edge{{From: "base", To: "nope", Action: ActionBack}}}},
		{"unknown anchor", &Graph{Entry: "base", Edges: []Edge{{From: "base", To: "alliance", Action: ActionTap, AnchorID: "nope"}}}},
		{"tap without anchor", &Graph{Entry: "base", Edges: []Edge{{From: "base", To: "alliance", Action: ActionTap}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.g.Validate(reg); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
}

func TestGraphPath(t *testing.T) {
	g := &Graph{Entry: "a", Edges: []Edge{
		{From: "a", To: "b", Action: ActionBack},
		{From: "b", To: "c", Action: ActionBack},
		{From: "a", To: "c", Action: ActionBack}, // direct shortcut
	}}
	path, err := g.Path("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 1 || path[0].To != "c" {
		t.Fatalf("path: got %+v, want the direct a->c edge", path)
	}

	if _, err := g.Path("c", "a"); !errors.Is(err, ErrNoPath) {
		t.Fatalf("unreachable: got %v, want ErrNoPath", err)
	}

	path, err = g.Path("b", "b")
	if err != nil || len(path) != 0 {
		t.Fatalf("self path: got %+v, %v; want empty, nil", path, err)
	}
}

func TestDefaultGraphShape(t *testing.T) {
	g := DefaultGraph()
	if g.Entry != "base" {
		t.Fatalf("entry: got %q, want base", g.Entry)
	}
	// Every Tier 1 task screen must be reachable from the entry screen.
	for _, target := range []string{"alliance", "alliance_tech", "mail", "radar"} {
		if _, err := g.Path(g.Entry, target); err != nil {
			t.Errorf("no path from %q to %q: %v", g.Entry, target, err)
		}
		if _, err := g.Path(target, g.Entry); err != nil {
			t.Errorf("no path back from %q: %v", target, err)
		}
	}
}
