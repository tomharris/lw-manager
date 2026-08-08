package runtime

import (
	"errors"
	"fmt"

	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrNoPath reports that the screen graph has no route between two screens.
var ErrNoPath = errors.New("runtime: no path between screens")

// ActionKind is what an edge does to move between screens.
type ActionKind int

const (
	// ActionTap taps a named anchor on the From screen.
	ActionTap ActionKind = iota
	// ActionBack presses the Android back button.
	ActionBack
)

// Edge is one navigation step: from one screen to another via an action.
type Edge struct {
	From, To string
	Action   ActionKind
	AnchorID string // required for ActionTap
}

// Graph is the navigation topology. It is data, validated against the
// template registry at Ctx construction: an edge naming a screen or anchor
// the registry does not know is a bug that must fail loudly at startup, not
// surface as a mysterious mid-task miss.
type Graph struct {
	// Entry is the screen the game lands on after an app restart — the
	// panic route's final waypoint.
	Entry string
	Edges []Edge
}

// Validate checks every edge against the loaded registry.
func (g *Graph) Validate(reg *vision.Registry) error {
	if _, ok := reg.Screen(g.Entry); !ok {
		return fmt.Errorf("runtime: graph entry screen %q not in registry", g.Entry)
	}
	for _, e := range g.Edges {
		from, ok := reg.Screen(e.From)
		if !ok {
			return fmt.Errorf("runtime: graph edge %q->%q: screen %q not in registry", e.From, e.To, e.From)
		}
		if _, ok := reg.Screen(e.To); !ok {
			return fmt.Errorf("runtime: graph edge %q->%q: screen %q not in registry", e.From, e.To, e.To)
		}
		if e.Action == ActionTap {
			if e.AnchorID == "" {
				return fmt.Errorf("runtime: graph edge %q->%q: tap edge without an anchor", e.From, e.To)
			}
			found := false
			for _, a := range from.Anchors {
				if a.ID == e.AnchorID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("runtime: graph edge %q->%q: anchor %q not on screen %q", e.From, e.To, e.AnchorID, e.From)
			}
		}
	}
	return nil
}

// Path returns the shortest edge sequence from one screen to another (BFS,
// deterministic by edge order). An empty path means from == to.
func (g *Graph) Path(from, to string) ([]Edge, error) {
	if from == to {
		return nil, nil
	}
	type node struct {
		screen string
		path   []Edge
	}
	visited := map[string]bool{from: true}
	queue := []node{{screen: from}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.Edges {
			if e.From != cur.screen || visited[e.To] {
				continue
			}
			path := append(append([]Edge{}, cur.path...), e)
			if e.To == to {
				return path, nil
			}
			visited[e.To] = true
			queue = append(queue, node{screen: e.To, path: path})
		}
	}
	return nil, fmt.Errorf("runtime: %q -> %q: %w", from, to, ErrNoPath)
}

// DefaultGraph is the production topology for the Tier 1 tasks.
//
// Two screens are reachable only by task code and appear here with out-edges
// only: alliance_tech_donate and stamina_prompt. Both are entered by tapping
// something whose effect depends on state the graph cannot see, and the graph
// models routes that are always available.
//
// An in-edge to the donate dialog would name an anchor that is absent whenever
// the recommendation sits on the unselected tab. An in-edge to the stamina
// prompt would be worse: it would name quick_execute_button, a control that
// usually starts an execution and sometimes opens a purchase dialog. The graph
// must not contain a route whose traversal is a purchase.
//
// The rewards celebration needs no edges at all. It plays over a screen that
// stays fully recognizable, so nothing is ever stranded on it and NavigateTo
// is never confused by it.
//
// alliance_members and the vs tree stay unrouted: they are recognized for
// M4's benefit, and navigating them is M4's problem.
func DefaultGraph() *Graph {
	return &Graph{
		Entry: vision.ScreenBase,
		Edges: []Edge{
			{From: vision.ScreenBase, To: vision.ScreenWorldMap, Action: ActionTap, AnchorID: "world_map_button"},
			{From: vision.ScreenWorldMap, To: vision.ScreenBase, Action: ActionTap, AnchorID: "base_button"},

			{From: vision.ScreenBase, To: vision.ScreenAlliance, Action: ActionTap, AnchorID: "alliance_button"},
			{From: vision.ScreenAlliance, To: vision.ScreenBase, Action: ActionBack},
			{From: vision.ScreenAlliance, To: vision.ScreenAllianceTech, Action: ActionTap, AnchorID: "tech_button"},
			{From: vision.ScreenAllianceTech, To: vision.ScreenAlliance, Action: ActionBack},
			{From: vision.ScreenAllianceTechDonate, To: vision.ScreenAllianceTech, Action: ActionBack},

			{From: vision.ScreenBase, To: vision.ScreenMail, Action: ActionTap, AnchorID: "mail_button"},
			{From: vision.ScreenMail, To: vision.ScreenBase, Action: ActionBack},
			{From: vision.ScreenMail, To: vision.ScreenMailAlliance, Action: ActionTap, AnchorID: "mail_alliance_button"},
			{From: vision.ScreenMailAlliance, To: vision.ScreenMail, Action: ActionBack},
			{From: vision.ScreenMail, To: vision.ScreenMailEvent, Action: ActionTap, AnchorID: "mail_event_button"},
			{From: vision.ScreenMailEvent, To: vision.ScreenMail, Action: ActionBack},
			{From: vision.ScreenMail, To: vision.ScreenMailSystem, Action: ActionTap, AnchorID: "mail_system_button"},
			{From: vision.ScreenMailSystem, To: vision.ScreenMail, Action: ActionBack},

			{From: vision.ScreenBase, To: vision.ScreenRadar, Action: ActionTap, AnchorID: "radar_button"},
			{From: vision.ScreenRadar, To: vision.ScreenBase, Action: ActionBack},

			{From: vision.ScreenStaminaPrompt, To: vision.ScreenRadar, Action: ActionBack},
		},
	}
}
