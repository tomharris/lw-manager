package vision

import (
	"errors"
	"fmt"
	"image"
	"math"
)

// Frame is one decoded corpus image with its true label.
type Frame struct {
	Hash  string
	Label string
	Image image.Image
}

// Evaluate runs the recognizer over every frame and, separately, scores every
// anchor against every frame.
//
// The two outputs answer different questions. Predictions say whether the
// recognizer is right; observations say which anchor to fix when it is not.
// Both are plain tuples, so the interpretation in Score and Separations stays
// pure and this function stays the only slow, image-shaped part.
//
// Both come out of a single pass of the matcher. Scoring the anchors a second
// time to build the observations would double the only expensive work here —
// at corpus scale that is thousands of redundant NCC sweeps — and would risk
// the report describing anchors the prediction never actually consulted.
func Evaluate(reg *Registry, frames []Frame) ([]Prediction, []AnchorObservation, error) {
	rec := NewRecognizer(reg)
	preds := make([]Prediction, 0, len(frames))
	var obs []AnchorObservation

	for _, f := range frames {
		screen, _, scored, err := rec.recognize(f.Image)
		switch {
		case errors.Is(err, ErrNoScreenRecognized):
			screen = ""
		case err != nil:
			return nil, nil, fmt.Errorf("vision: recognizing frame %s (label %q): %w", f.Hash, f.Label, err)
		}
		preds = append(preds, Prediction{Hash: f.Hash, Label: f.Label, Predicted: screen})

		for _, sa := range scored {
			obs = append(obs, AnchorObservation{
				AnchorID:   sa.AnchorID,
				Screen:     sa.Screen,
				FrameLabel: f.Label,
				Score:      sa.Score,
			})
		}
	}
	return preds, obs, nil
}

// Rescale returns img scaled by factor, in grayscale.
func Rescale(img image.Image, factor float64) image.Image {
	b := img.Bounds()
	w := int(math.Round(float64(b.Dx()) * factor))
	h := int(math.Round(float64(b.Dy()) * factor))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return Resize(img, w, h)
}
