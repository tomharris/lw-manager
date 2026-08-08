package tasks

import (
	"context"
	"errors"
	"image"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/vision"
)

// toAllianceTech is the six frames NavigateTo spends walking
// base -> alliance -> alliance_tech: the opening CurrentScreen, then a tap
// verify and a WaitFor per hop, then the CurrentScreen that confirms arrival.
// The caller supplies the alliance_tech frame, because which badges it renders
// is the whole subject of these tests.
func toAllianceTech(tech image.Image) []image.Image {
	base := runtimetest.Frame(vision.ScreenBase)
	alliance := runtimetest.Frame(vision.ScreenAlliance)
	return []image.Image{base, base, alliance, alliance, tech, tech}
}

// The recommendation is on the selected tab: one tap opens the dialog.
func TestTechDonateFindsTheRecommendationOnTheSelectedTab(t *testing.T) {
	tech := runtimetest.FrameWithout(vision.ScreenAllianceTech, "tab_recommended_badge")
	donate := runtimetest.Frame(vision.ScreenAllianceTechDonate)
	empty := runtimetest.FrameWithout(vision.ScreenAllianceTechDonate, "donate_button")

	frames := toAllianceTech(tech)
	frames = append(frames, tech)              // the tech-list badge check, which hits
	frames = append(frames, rep(donate, 4)...) // WaitFor, then three donate taps
	frames = append(frames, rep(empty, 2)...)  // the button is gone: stop
	rt, tr := ctxFor(t, frames...)

	fn, ok := Get("tech_donate")
	if !ok {
		t.Fatal("tech_donate not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("tech_donate: %v", err)
	}
	// Two navigation taps, the badge, and three donations.
	if taps := countTaps(tr); taps != 6 {
		t.Errorf("got %d taps, want 6 (2 navigation, 1 badge, 3 donations)", taps)
	}
}

// The recommendation is on the other tab. Its header carries the badge —
// but only while unselected — so the task taps that, then retries the list.
func TestTechDonateSwitchesTabsWhenTheRecommendationIsElsewhere(t *testing.T) {
	onTab := runtimetest.FrameWithout(vision.ScreenAllianceTech, "tech_recommended_badge")
	inList := runtimetest.FrameWithout(vision.ScreenAllianceTech, "tab_recommended_badge")
	donate := runtimetest.Frame(vision.ScreenAllianceTechDonate)
	empty := runtimetest.FrameWithout(vision.ScreenAllianceTechDonate, "donate_button")

	frames := toAllianceTech(onTab)
	frames = append(frames, onTab, onTab)      // the list check misses, the tab check hits
	frames = append(frames, inList)            // after the switch, the list has it
	frames = append(frames, rep(donate, 4)...) // WaitFor, then three donate taps
	frames = append(frames, rep(empty, 2)...)
	rt, tr := ctxFor(t, frames...)

	fn, _ := Get("tech_donate")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("tech_donate across tabs: %v", err)
	}
	// The tab switch costs one extra tap over the selected-tab case.
	if taps := countTaps(tr); taps != 7 {
		t.Errorf("got %d taps, want 7 (2 navigation, 1 tab, 1 badge, 3 donations)", taps)
	}
}

// No badge in the list and none on a tab header means nothing is
// recommended. That is success — and it is the one reading that cannot be
// got from a single check, because the tab badge is suppressed exactly when
// the recommendation is on the tab you are already looking at.
func TestTechDonateSucceedsWhenNothingIsRecommended(t *testing.T) {
	none := runtimetest.FrameWithout(vision.ScreenAllianceTech,
		"tech_recommended_badge", "tab_recommended_badge")

	frames := append(toAllianceTech(none), rep(none, 4)...)
	rt, tr := ctxFor(t, frames...)

	fn, _ := Get("tech_donate")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("no recommendation must be success: %v", err)
	}
	if taps := countTaps(tr); taps != 2 {
		t.Errorf("got %d taps, want only the 2 navigation taps", taps)
	}
}

// The donate button greys out rather than vanishing, so a failed
// discriminator means it matches forever. The cap must stop that, loudly.
func TestTechDonateFailsWhenTheDonateButtonNeverStopsMatching(t *testing.T) {
	tech := runtimetest.FrameWithout(vision.ScreenAllianceTech, "tab_recommended_badge")
	donate := runtimetest.Frame(vision.ScreenAllianceTechDonate)

	frames := append(toAllianceTech(tech), tech)
	frames = append(frames, rep(donate, 3*maxDonations)...)
	rt, _ := ctxFor(t, frames...)

	fn, _ := Get("tech_donate")
	if err := fn(context.Background(), rt); !errors.Is(err, ErrTapCapReached) {
		t.Fatalf("got %v, want ErrTapCapReached", err)
	}
}
