package vision_test

import (
	"testing"

	"github.com/tomharris/lw-manager/internal/vision"
)

func TestScreenNamesContainsEveryDeclaredConstant(t *testing.T) {
	// Every constant must be in the list: a name declared but left out of
	// ScreenNames is invisible to the studio's label set and to the M1
	// gate's per-label frame-count check, which is exactly the silent
	// drift screens.go exists to prevent.
	want := []string{
		vision.ScreenAlliance, vision.ScreenAllianceDuel, vision.ScreenAllianceMembers,
		vision.ScreenAllianceTech, vision.ScreenAllianceTechDonate, vision.ScreenBase,
		vision.ScreenMail, vision.ScreenMailAlliance, vision.ScreenMailEvent,
		vision.ScreenMailSystem, vision.ScreenRadar, vision.ScreenStaminaPrompt,
		vision.ScreenVS, vision.ScreenVSRanking, vision.ScreenVSRankingWeekly,
		vision.ScreenWorldMap,
	}
	got := make(map[string]bool, len(vision.ScreenNames))
	for _, n := range vision.ScreenNames {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("ScreenNames is missing %q", w)
		}
	}
	if len(vision.ScreenNames) != len(want) {
		t.Errorf("ScreenNames has %d entries, want %d", len(vision.ScreenNames), len(want))
	}
}

func TestScreenNamesHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range vision.ScreenNames {
		if seen[n] {
			t.Errorf("duplicate screen name %q", n)
		}
		seen[n] = true
	}
}
