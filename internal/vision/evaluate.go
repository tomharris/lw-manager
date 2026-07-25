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
func Evaluate(reg *Registry, frames []Frame) ([]Prediction, []AnchorObservation, error) {
	rec := NewRecognizer(reg)
	preds := make([]Prediction, 0, len(frames))
	var obs []AnchorObservation

	for _, f := range frames {
		screen, _, err := rec.Recognize(f.Image)
		switch {
		case errors.Is(err, ErrNoScreenRecognized):
			screen = ""
		case err != nil:
			return nil, nil, fmt.Errorf("vision: recognizing frame %s (label %q): %w", f.Hash, f.Label, err)
		}
		preds = append(preds, Prediction{Hash: f.Hash, Label: f.Label, Predicted: screen})

		for _, s := range reg.Screens {
			for _, a := range s.Anchors {
				if !a.IdentifiesScreen {
					continue
				}
				m, err := Match(f.Image, a.Template, a.Region, reg.ReferenceHeight)
				if err != nil {
					return nil, nil, fmt.Errorf("vision: matching %s/%s against frame %s: %w",
						s.Name, a.ID, f.Hash, err)
				}
				obs = append(obs, AnchorObservation{
					AnchorID:   a.ID,
					Screen:     s.Name,
					FrameLabel: f.Label,
					Score:      m.Score,
				})
			}
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
