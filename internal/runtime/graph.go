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

// DefaultGraph is the production topology for the Tier 1 tasks. The anchor
// IDs name templates that will exist once the real corpus is captured; until
// then Validate rejects this graph against the shipping manifest, which is
// the designed behavior — skeleton tasks must refuse to run rather than
// blind-tap unproven screens.
func DefaultGraph() *Graph {
	return &Graph{
		Entry: "base",
		Edges: []Edge{
			{From: "base", To: "world_map", Action: ActionTap, AnchorID: "world_map_button"},
			{From: "world_map", To: "base", Action: ActionTap, AnchorID: "base_button"},
			{From: "base", To: "alliance", Action: ActionTap, AnchorID: "alliance_button"},
			{From: "alliance", To: "base", Action: ActionBack},
			{From: "alliance", To: "alliance_tech", Action: ActionTap, AnchorID: "tech_button"},
			{From: "alliance_tech", To: "alliance", Action: ActionBack},
			{From: "base", To: "mail", Action: ActionTap, AnchorID: "mail_button"},
			{From: "mail", To: "base", Action: ActionBack},
			{From: "base", To: "radar", Action: ActionTap, AnchorID: "radar_button"},
			{From: "radar", To: "base", Action: ActionBack},
		},
	}
}
