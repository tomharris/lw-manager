package vision

import (
	"fmt"
	"strings"
)

// ErrUnknownScreen reports that a caller named a screen the registry does not
// have — e.g. a typo passed to `agent score --actions --screen`.
var ErrUnknownScreen = fmt.Errorf("vision: unknown screen")

// screenAnchor pairs an action anchor with the screen that declares it. It is
// the action-scan analogue of scoredAnchor in recognizer.go: recognition
// already knows which screen an anchor belongs to because it walks
// reg.Screens screen-by-screen, but ActionAnchors needs to carry that
// association explicitly once it has flattened anchors out of their screens.
type screenAnchor struct {
	Screen string
	Anchor Anchor
}

// ActionAnchors returns every action anchor (IdentifiesScreen == false) in
// the registry, or — when screen is non-empty — only those declared on that
// one screen.
//
// This is deliberately the same filter EvaluateActions uses to decide what to
// score: keeping the anchor selection and the scoring loop as two separate,
// narrow functions means `agent score --actions --screen X` can report which
// anchors it is about to measure before paying for the scan, and a test can
// assert the selection is right without decoding a single frame.
//
// An unknown screen name is a caller error worth failing loudly on — silently
// scanning zero anchors would print an empty, misleadingly-clean report
// instead of telling the operator they mistyped the name.
func ActionAnchors(reg *Registry, screen string) ([]screenAnchor, error) {
	if screen != "" {
		s, ok := reg.Screen(screen)
		if !ok {
			names := make([]string, len(reg.Screens))
			for i, sc := range reg.Screens {
				names[i] = sc.Name
			}
			return nil, fmt.Errorf("%w: %q; valid screens: %s", ErrUnknownScreen, screen, strings.Join(names, ", "))
		}
		return actionAnchorsOf(s), nil
	}

	var out []screenAnchor
	for _, s := range reg.Screens {
		out = append(out, actionAnchorsOf(s)...)
	}
	return out, nil
}

func actionAnchorsOf(s Screen) []screenAnchor {
	var out []screenAnchor
	for _, a := range s.Anchors {
		if a.IdentifiesScreen {
			continue
		}
		out = append(out, screenAnchor{Screen: s.Name, Anchor: a})
	}
	return out
}

// EvaluateActions scores action anchors against every frame, producing the
// same AnchorObservation shape Evaluate produces for identifying anchors —
// so Separations, already trusted for the recognition report, needs no
// change to make sense of these too.
//
// This is a separate pass rather than a mode of Evaluate/Recognize on
// purpose: invariant #3 is that no task acts without a matched identifying
// anchor, and the recognizer must keep ignoring action anchors when deciding
// which screen a frame shows (recognizer.go:69). Folding action scoring into
// that path would risk an action anchor's score leaking into a recognition
// decision it was never meant to influence. This function never touches
// Recognizer or Evaluate; it matches action anchors directly and only reuses
// the observation/separation types they already produce.
//
// An action anchor's in-screen observations are frames labelled with the
// screen that declares it; every other frame is out-of-screen. That is
// exactly the semantics Separations already assumes for identifying anchors,
// which is why no new aggregation logic is needed here — only a new source
// of observations for it to aggregate.
//
// screen narrows the scan to one screen's action anchors (see ActionAnchors).
// Passing "" scores every action anchor in the registry, which is the
// expensive path: 38 anchors against a 620-frame corpus is roughly double the
// identifying-anchor pass, because every action anchor is matched against
// every frame regardless of that frame's label — out-of-screen frames are
// exactly where a false positive would show up. Naming one screen cuts the
// anchor count to that screen's handful, not the frame count: an anchor's
// out-of-screen observations still need every other frame to be meaningful.
func EvaluateActions(reg *Registry, frames []Frame, screen string) ([]AnchorObservation, error) {
	anchors, err := ActionAnchors(reg, screen)
	if err != nil {
		return nil, err
	}

	var obs []AnchorObservation
	for _, sa := range anchors {
		for _, f := range frames {
			m, err := Match(f.Image, sa.Anchor.Template, sa.Anchor.Region, reg.ReferenceHeight)
			if err != nil {
				return nil, fmt.Errorf("vision: matching action anchor %q on screen %q against frame %s (label %q): %w",
					sa.Anchor.ID, sa.Screen, f.Hash, f.Label, err)
			}
			obs = append(obs, AnchorObservation{
				AnchorID:   sa.Anchor.ID,
				Screen:     sa.Screen,
				FrameLabel: f.Label,
				Score:      m.Score,
			})
		}
	}
	return obs, nil
}
