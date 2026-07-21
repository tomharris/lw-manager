package vision

import (
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// fullRegion is a search region covering the whole frame.
var fullRegion = transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}

// twoScreenRegistry: "world_map" is identified by patternA, "alliance" by a
// different patternB. Both templates are captured at refHeight 64.
func twoScreenRegistry() *Registry {
	patternA := distinctTemplate(8, 8)
	patternB := invertGray(distinctTemplate(8, 8)) // a genuinely different pattern
	return &Registry{
		ReferenceHeight: 64,
		Screens: []Screen{
			{Name: "alliance", Anchors: []Anchor{
				{ID: "b", Template: patternB, Region: fullRegion, Threshold: 0.9, IdentifiesScreen: true},
			}},
			{Name: "world_map", Anchors: []Anchor{
				{ID: "a", Template: patternA, Region: fullRegion, Threshold: 0.9, IdentifiesScreen: true},
			}},
		},
	}
}

func invertGray(g *image.Gray) *image.Gray { return Invert(g) }

func TestRecognizePicksMatchingScreen(t *testing.T) {
	reg := twoScreenRegistry()
	frame := paste(64, 64, 100, distinctTemplate(8, 8), 20, 30) // contains patternA

	rec := NewRecognizer(reg)
	screen, conf, err := rec.Recognize(frame)
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if screen != "world_map" {
		t.Errorf("screen: got %q, want world_map", screen)
	}
	if conf < 0.9 {
		t.Errorf("confidence: got %.4f, want ≥0.9", conf)
	}
}

func TestRecognizeReturnsSentinelWhenNothingMatches(t *testing.T) {
	reg := twoScreenRegistry()
	blank := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			blank.SetGray(x, y, color.Gray{Y: 100})
		}
	}

	rec := NewRecognizer(reg)
	_, _, err := rec.Recognize(blank)
	if !errors.Is(err, ErrNoScreenRecognized) {
		t.Fatalf("expected ErrNoScreenRecognized, got %v", err)
	}
}
