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

// Anchors are declared per-screen (registry.go's screenManifest.Anchors is
// independent per screen name) with no global uniqueness constraint, so two
// screens can legitimately both have a back_button. Separations must key on
// (AnchorID, Screen), not AnchorID alone, or a clean anchor on one screen
// would merge with — and mask — a non-discriminative namesake on another.
func TestSeparationsKeysByAnchorAndScreenNotAnchorAlone(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "back_button", Screen: "alliance", FrameLabel: "alliance", Score: 0.9},
		{AnchorID: "back_button", Screen: "alliance", FrameLabel: "mail", Score: 0.1},
		{AnchorID: "back_button", Screen: "mail", FrameLabel: "mail", Score: 0.95},
		{AnchorID: "back_button", Screen: "mail", FrameLabel: "alliance", Score: 0.05},
	})

	if len(seps) != 2 {
		t.Fatalf("Separations = %+v, want two rows (one per screen)", seps)
	}
	byScreen := map[string]vision.Separation{}
	for _, s := range seps {
		byScreen[s.Screen] = s
	}

	alliance, ok := byScreen["alliance"]
	if !ok {
		t.Fatalf("no row for back_button@alliance: %+v", seps)
	}
	if alliance.InCount != 1 || alliance.OutCount != 1 {
		t.Fatalf("alliance InCount/OutCount = %d/%d, want 1/1", alliance.InCount, alliance.OutCount)
	}
	if math.Abs(alliance.WorstIn-0.9) > 1e-9 || math.Abs(alliance.BestOut-0.1) > 1e-9 {
		t.Fatalf("alliance WorstIn/BestOut = %v/%v, want 0.9/0.1", alliance.WorstIn, alliance.BestOut)
	}

	mail, ok := byScreen["mail"]
	if !ok {
		t.Fatalf("no row for back_button@mail: %+v", seps)
	}
	if mail.InCount != 1 || mail.OutCount != 1 {
		t.Fatalf("mail InCount/OutCount = %d/%d, want 1/1", mail.InCount, mail.OutCount)
	}
	if math.Abs(mail.WorstIn-0.95) > 1e-9 || math.Abs(mail.BestOut-0.05) > 1e-9 {
		t.Fatalf("mail WorstIn/BestOut = %v/%v, want 0.95/0.05", mail.WorstIn, mail.BestOut)
	}
}

// The case that matters: a namesake anchor on one screen must not rescue a
// non-discriminative anchor of the same name on another screen.
func TestSeparationsDoesNotLetAHealthyNamesakeMaskAnOverlappingAnchor(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		// back_button@mail overlaps: worst-in (0.71) is below best-out (0.77).
		{AnchorID: "back_button", Screen: "mail", FrameLabel: "mail", Score: 0.71},
		{AnchorID: "back_button", Screen: "mail", FrameLabel: "base", Score: 0.77},
		// back_button@alliance is cleanly separable.
		{AnchorID: "back_button", Screen: "alliance", FrameLabel: "alliance", Score: 0.9},
		{AnchorID: "back_button", Screen: "alliance", FrameLabel: "mail", Score: 0.1},
	})

	if len(seps) != 2 {
		t.Fatalf("Separations = %+v, want two rows", seps)
	}
	byScreen := map[string]vision.Separation{}
	for _, s := range seps {
		byScreen[s.Screen] = s
	}

	if mail := byScreen["mail"]; !mail.Overlap {
		t.Fatalf("back_button@mail = %+v, want Overlap true — must not be rescued by its alliance namesake", mail)
	}
	if alliance := byScreen["alliance"]; alliance.Overlap {
		t.Fatalf("back_button@alliance = %+v, want Overlap false", alliance)
	}
}

// Two anchors that share an AnchorID but differ by Screen must still sort
// deterministically — AnchorID first, Screen as the tiebreak — so the report
// is stable across runs regardless of map iteration order.
func TestSeparationsIsSortedByAnchorIDThenScreenWhenIDsTie(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "back_button", Screen: "mail", FrameLabel: "mail", Score: 0.9},
		{AnchorID: "back_button", Screen: "alliance", FrameLabel: "alliance", Score: 0.9},
		{AnchorID: "back_button", Screen: "radar", FrameLabel: "radar", Score: 0.9},
	})

	if len(seps) != 3 {
		t.Fatalf("Separations = %+v, want three rows", seps)
	}
	want := []string{"alliance", "mail", "radar"}
	for i, s := range seps {
		if s.Screen != want[i] {
			t.Fatalf("Separations screens = %v, want sorted %v", screensOf(seps), want)
		}
	}
}

func screensOf(seps []vision.Separation) []string {
	out := make([]string, len(seps))
	for i, s := range seps {
		out[i] = s.Screen
	}
	return out
}

func TestFormatSeparationsMarksTheAnchorsThatNeedRecropping(t *testing.T) {
	out := vision.FormatSeparations(vision.Separations([]vision.AnchorObservation{
		// Overlapping: no threshold works.
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "mail", Score: 0.71},
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "base", Score: 0.77},
		// Healthy: cleanly separable, must not be flagged and must carry a
		// real suggested threshold rather than a hardcoded placeholder.
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "alliance", Score: 0.9},
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "mail", Score: 0.1},
	}))

	if !strings.Contains(out, "mail_button") {
		t.Fatalf("report does not name the overlapping anchor:\n%s", out)
	}
	if !strings.Contains(strings.ToUpper(out), "RECROP") {
		t.Fatalf("report does not say what action an overlap needs:\n%s", out)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	var healthyLine string
	for _, l := range lines {
		if strings.Contains(l, "alliance_button") {
			healthyLine = l
		}
	}
	if healthyLine == "" {
		t.Fatalf("report does not name the healthy anchor:\n%s", out)
	}
	if strings.Contains(strings.ToUpper(healthyLine), "RECROP") {
		t.Fatalf("healthy anchor's row wrongly says RECROP:\n%s", healthyLine)
	}
	// alliance_button's midpoint is (0.9+0.1)/2 = 0.5 — assert the real
	// number appears, not just that some row exists, so a stub that always
	// emits a fixed string cannot pass.
	if !strings.Contains(healthyLine, "0.500") {
		t.Fatalf("healthy anchor's row does not carry its suggested threshold:\n%s", healthyLine)
	}
}
