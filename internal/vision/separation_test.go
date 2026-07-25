package vision_test

import (
	"math"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/vision"
)

func TestSeparationsSuggestsTheMidpointOfASeparableGap(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "alliance", Score: 0.94},
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "alliance", Score: 0.88},
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "mail", Score: 0.42},
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "_none", Score: 0.51},
	})

	if len(seps) != 1 {
		t.Fatalf("Separations = %+v, want one anchor", seps)
	}
	s := seps[0]
	if s.InCount != 2 || s.OutCount != 2 {
		t.Fatalf("InCount/OutCount = %d/%d, want 2/2", s.InCount, s.OutCount)
	}
	if math.Abs(s.WorstIn-0.88) > 1e-9 || math.Abs(s.BestOut-0.51) > 1e-9 {
		t.Fatalf("WorstIn/BestOut = %v/%v, want 0.88/0.51", s.WorstIn, s.BestOut)
	}
	if math.Abs(s.Gap-0.37) > 1e-9 {
		t.Fatalf("Gap = %v, want 0.37", s.Gap)
	}
	if math.Abs(s.Suggested-0.695) > 1e-9 {
		t.Fatalf("Suggested = %v, want 0.695", s.Suggested)
	}
	if s.Overlap {
		t.Fatal("a separable anchor was flagged as overlapping")
	}
}

// The case the whole report exists for: no threshold can work, so retuning is
// the wrong action and recropping is the right one.
func TestSeparationsFlagsOverlapWhenNoThresholdCanSeparate(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "mail", Score: 0.71},
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "base", Score: 0.77},
	})

	s := seps[0]
	if !s.Overlap {
		t.Fatalf("Overlap = false for gap %v, want true", s.Gap)
	}
	if s.Suggested != 0 {
		t.Fatalf("Suggested = %v, want 0 for an unseparable anchor", s.Suggested)
	}
}

// An anchor never seen on its own screen cannot be thresholded either. It is
// a corpus gap, and must not be reported as a healthy wide margin.
func TestSeparationsTreatsNoInScreenObservationsAsOverlap(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "vs_button", Screen: "vs_ranking", FrameLabel: "base", Score: 0.2},
	})

	s := seps[0]
	if s.InCount != 0 {
		t.Fatalf("InCount = %d, want 0", s.InCount)
	}
	if !s.Overlap {
		t.Fatal("an anchor with no in-screen observations was not flagged")
	}
}

// NCC scores floor at 0, so an anchor never seen off its screen gets a
// well-defined margin rather than an infinite one.
func TestSeparationsWithNoOutOfScreenObservationsUsesZeroAsTheFloor(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "radar_button", Screen: "radar", FrameLabel: "radar", Score: 0.9},
	})

	s := seps[0]
	if s.BestOut != 0 {
		t.Fatalf("BestOut = %v, want 0", s.BestOut)
	}
	if math.Abs(s.Suggested-0.45) > 1e-9 {
		t.Fatalf("Suggested = %v, want 0.45", s.Suggested)
	}
	if s.Overlap {
		t.Fatal("a never-confused anchor was flagged as overlapping")
	}
}

func TestSeparationsIsSortedByAnchorID(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "z", Screen: "radar", FrameLabel: "radar", Score: 0.9},
		{AnchorID: "a", Screen: "mail", FrameLabel: "mail", Score: 0.9},
	})

	if len(seps) != 2 || seps[0].AnchorID != "a" || seps[1].AnchorID != "z" {
		t.Fatalf("Separations = %+v, want sorted by AnchorID", seps)
	}
}

func TestFormatSeparationsMarksTheAnchorsThatNeedRecropping(t *testing.T) {
	out := vision.FormatSeparations(vision.Separations([]vision.AnchorObservation{
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "mail", Score: 0.71},
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "base", Score: 0.77},
	}))

	if !strings.Contains(out, "mail_button") {
		t.Fatalf("report does not name the anchor:\n%s", out)
	}
	if !strings.Contains(strings.ToUpper(out), "RECROP") {
		t.Fatalf("report does not say what action an overlap needs:\n%s", out)
	}
}
