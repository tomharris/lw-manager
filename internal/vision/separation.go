package vision

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

// AnchorObservation is one anchor's NCC score against one corpus frame.
//
// Deliberately not named anchorScore: that identifier already belongs to the
// recognition decision's own type in recognizer.go.
type AnchorObservation struct {
	AnchorID   string
	Screen     string // the screen the anchor belongs to
	FrameLabel string // the frame's true label
	Score      float64
}

// Separation is one anchor's discriminative margin over the corpus.
type Separation struct {
	AnchorID  string
	Screen    string
	InCount   int
	OutCount  int
	WorstIn   float64
	BestOut   float64
	Gap       float64 // WorstIn - BestOut
	Suggested float64 // midpoint of the gap; 0 when Overlap
	Overlap   bool    // no threshold can separate: recrop, do not retune
}

// Separations computes one Separation per (anchor, screen) pair.
//
// The gap between the worst in-screen score and the best out-of-screen score
// is the anchor's discriminative margin. A positive gap means a threshold
// exists and the midpoint is the safest choice. A non-positive gap means no
// threshold can work — which calls for a different action entirely, so it is
// flagged rather than papered over with a suggested number.
//
// An anchor with no in-screen observations is treated as overlapping. It is a
// corpus gap, and reporting it as a healthy wide margin would be worse than
// useless.
func Separations(obs []AnchorObservation) []Separation {
	type key struct {
		anchorID string
		screen   string
	}
	type acc struct {
		worstIn  float64
		bestOut  float64
		inCount  int
		outCount int
	}
	// registry.go declares anchors per-screen with no global uniqueness
	// constraint, so the same AnchorID can legitimately name a different
	// anchor on a different screen (e.g. two screens each with a
	// back_button). Keying on AnchorID alone would blend those anchors'
	// scores together and let a healthy one mask a non-discriminative
	// namesake — exactly the papering-over this report exists to expose.
	byAnchor := map[key]*acc{}

	for _, o := range obs {
		k := key{anchorID: o.AnchorID, screen: o.Screen}
		a := byAnchor[k]
		if a == nil {
			// NCC scores floor at 0, so an anchor never seen off its own
			// screen gets a well-defined margin instead of an infinite one.
			a = &acc{worstIn: 1, bestOut: 0}
			byAnchor[k] = a
		}
		if o.FrameLabel == o.Screen {
			a.inCount++
			if o.Score < a.worstIn {
				a.worstIn = o.Score
			}
			continue
		}
		a.outCount++
		if o.Score > a.bestOut {
			a.bestOut = o.Score
		}
	}

	out := make([]Separation, 0, len(byAnchor))
	for k, a := range byAnchor {
		s := Separation{
			AnchorID: k.anchorID,
			Screen:   k.screen,
			InCount:  a.inCount,
			OutCount: a.outCount,
			BestOut:  a.bestOut,
		}
		if a.inCount == 0 {
			s.Overlap = true
			out = append(out, s)
			continue
		}
		s.WorstIn = a.worstIn
		s.Gap = a.worstIn - a.bestOut
		if s.Gap <= 0 {
			s.Overlap = true
		} else {
			s.Suggested = (a.worstIn + a.bestOut) / 2
		}
		out = append(out, s)
	}
	// Screen is the tiebreak so ordering is a total order — otherwise two
	// anchors sharing an ID across screens would sort arbitrarily and the
	// report would vary between runs.
	sort.Slice(out, func(i, j int) bool {
		if out[i].AnchorID != out[j].AnchorID {
			return out[i].AnchorID < out[j].AnchorID
		}
		return out[i].Screen < out[j].Screen
	})
	return out
}

// FormatSeparations renders the separation report, naming the action each
// anchor needs rather than only its numbers.
func FormatSeparations(seps []Separation) string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "anchor\tscreen\tin\tout\tworst-in\tbest-out\tgap\tsuggested\taction")
	for _, s := range seps {
		action := "ok"
		suggested := fmt.Sprintf("%.3f", s.Suggested)
		switch {
		case s.InCount == 0:
			action, suggested = "RECROP (never seen on its own screen)", "-"
		case s.Overlap:
			action, suggested = "RECROP (distributions overlap)", "-"
		case s.Gap < 0.05:
			action = "narrow margin"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.3f\t%.3f\t%+.3f\t%s\t%s\n",
			s.AnchorID, s.Screen, s.InCount, s.OutCount,
			s.WorstIn, s.BestOut, s.Gap, suggested, action)
	}
	_ = tw.Flush()
	return sb.String()
}
