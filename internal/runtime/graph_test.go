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

// A node entered by tapping something that is only sometimes there gets
// out-edges only. Two different failures motivate it.
//
// For stamina_prompt an in-edge would be an instruction to spend money: it
// would name quick_execute_button, a control that usually starts an execution
// and sometimes opens a purchase dialog. A re-plan could then pick that edge
// as a path segment. The graph must not contain a route whose traversal is a
// purchase.
//
// For alliance_tech_donate an in-edge is a trap: it would name
// tech_recommended_badge, which is absent whenever the recommendation is on
// the unselected tab. walk only re-plans on ErrWrongScreen, so
// ErrAnchorNotFound would propagate and fail the task when the dialog was one
// tab-tap away.
func TestConditionalNodesHaveNoInEdges(t *testing.T) {
	conditional := map[string]bool{
		vision.ScreenStaminaPrompt:      true,
		vision.ScreenAllianceTechDonate: true,
	}
	for _, e := range DefaultGraph().Edges {
		if conditional[e.To] {
			t.Errorf("edge %q -> %q: conditional nodes must have out-edges only", e.From, e.To)
		}
	}
}

// The stamina dialog spends currency. No route may pass through it, because a
// re-plan that chose it as a path segment would be making a purchase to get
// somewhere.
func TestPathNeverRoutesThroughTheStaminaPrompt(t *testing.T) {
	g := DefaultGraph()
	for _, to := range []string{vision.ScreenMailAlliance, vision.ScreenBase, vision.ScreenAllianceTech} {
		path, err := g.Path(vision.ScreenRadar, to)
		if err != nil {
			t.Fatalf("Path(radar, %s): %v", to, err)
		}
		for _, e := range path {
			if e.From == vision.ScreenStaminaPrompt || e.To == vision.ScreenStaminaPrompt {
				t.Fatalf("Path(radar, %s) routed through the stamina prompt: %+v", to, path)
			}
		}
	}
}

// A crash on the stamina dialog must not strand the next task.
func TestTheStaminaPromptEscapesToRadar(t *testing.T) {
	if _, err := DefaultGraph().Path(vision.ScreenStaminaPrompt, vision.ScreenRadar); err != nil {
		t.Errorf("no route out of the stamina prompt: %v", err)
	}
}

func TestEveryMailboxIsReachableFromBase(t *testing.T) {
	g := DefaultGraph()
	for _, box := range []string{
		vision.ScreenMailAlliance, vision.ScreenMailEvent, vision.ScreenMailSystem,
	} {
		if _, err := g.Path(vision.ScreenBase, box); err != nil {
			t.Errorf("no route from base to %q: %v", box, err)
		}
	}
}

// The donate dialog is entered only by task code, but must always be
// escapable — a crash there would otherwise strand every later task.
func TestTheDonateDialogEscapesToAllianceTech(t *testing.T) {
	if _, err := DefaultGraph().Path(
		vision.ScreenAllianceTechDonate, vision.ScreenAllianceTech); err != nil {
		t.Errorf("no route out of the donate dialog: %v", err)
	}
}

func TestDefaultGraphValidatesAgainstTheShippingManifest(t *testing.T) {
	reg, err := vision.LoadRegistry("../../templates/manifest.yaml")
	if err != nil {
		t.Skipf("shipping manifest not loadable: %v", err)
	}
	if err := DefaultGraph().Validate(reg); err != nil {
		t.Fatalf("DefaultGraph must validate against the real templates: %v", err)
	}
}
