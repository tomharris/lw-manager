package vision

import (
	"errors"
	"fmt"
	"image"
)

// ErrNoScreenRecognized reports that no screen's identifying anchors all
// matched. This is the trigger for the task engine's panic route (spam back,
// restart, re-recognize) — never let an agent act on an unrecognized screen
// (invariant #3).
var ErrNoScreenRecognized = errors.New("vision: no screen recognized")

// Recognizer decides which known screen a frame shows, by matching each
// screen's identifying anchors and applying a confidence rule.
type Recognizer struct {
	reg *Registry
}

// NewRecognizer builds a recognizer over a loaded template library.
func NewRecognizer(reg *Registry) *Recognizer {
	return &Recognizer{reg: reg}
}

// anchorScore pairs one identifying anchor's NCC match against the threshold it
// had to clear. It is the input to the recognition decision.
type anchorScore struct {
	Score     float64
	Threshold float64
}

// Recognize returns the screen the frame most likely shows, with a confidence,
// or ErrNoScreenRecognized. It runs every identifying anchor of every screen
// through the matcher, then defers the actual decision — does a screen qualify,
// and how confident are we — to scoreScreen.
func (r *Recognizer) Recognize(img image.Image) (string, float64, error) {
	bestScreen := ""
	bestConf := -1.0

	for _, s := range r.reg.Screens {
		var scores []anchorScore
		for _, a := range s.Anchors {
			if !a.IdentifiesScreen {
				continue
			}
			m, err := Match(img, a.Template, a.Region, r.reg.ReferenceHeight)
			if err != nil {
				return "", 0, fmt.Errorf("vision: matching screen %q anchor %q: %w", s.Name, a.ID, err)
			}
			scores = append(scores, anchorScore{Score: m.Score, Threshold: a.Threshold})
		}

		recognized, conf := scoreScreen(scores)
		if recognized && conf > bestConf {
			bestConf = conf
			bestScreen = s.Name
		}
	}

	if bestScreen == "" {
		return "", 0, ErrNoScreenRecognized
	}
	return bestScreen, bestConf, nil
}

// scoreScreen decides whether a screen is recognized from its identifying
// anchors' scores, and with what confidence it should be ranked against other
// screens that also qualify.
//
// A screen is recognized only if every identifying anchor cleared its own
// threshold (all-anchors-match, per design doc §3); a screen with zero
// identifying anchors can never be recognized, since an empty anchors slice
// can't identify anything. Confidence is in [0,1] so Recognize can break
// ties between two screens that both qualify — it keeps the highest.
//
// The judgement call is the aggregation:
//   - min(scores) is conservative: confidence is only as high as the weakest
//     identifying anchor, so one shaky anchor can't inflate a shaky screen.
//   - mean(scores) is smoother but can let a strong anchor paper over a weak one.
//
// min is what's implemented below, and the tie-break behaviour falls out of
// it.
func scoreScreen(anchors []anchorScore) (recognized bool, confidence float64) {
	// A screen with no identifying anchors can identify nothing.
	if len(anchors) == 0 {
		return false, 0
	}
	// Min aggregation: the weakest anchor gates both qualification and
	// confidence, so one shaky match can't inflate a shaky screen.
	weakest := 1.0
	for _, a := range anchors {
		if a.Score < a.Threshold {
			return false, 0 // any anchor below its own threshold disqualifies
		}
		if a.Score < weakest {
			weakest = a.Score
		}
	}
	return true, weakest
}
