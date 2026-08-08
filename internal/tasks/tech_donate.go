package tasks

import (
	"context"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/vision"
)

// maxDonations bounds the donate loop. Set from the maximum donations
// observed in the capture session plus headroom, not from a round number.
// Reaching it is a bug — see tapUntilGone.
const maxDonations = 24

// tabSettle paces the wait after a tab switch. A var only so the device-free
// suite can collapse it; see TestMain.
var tabSettle = 500 * time.Millisecond

func init() { Register("tech_donate", techDonate) }

// tech_donate donates to the alliance's recommended tech.
//
// alliance_tech has two tabs and the recommendation may be on either. The
// tab holding it carries the same badge on its header — but only while that
// tab is unselected, since selecting it removes the badge. So "no tab
// badge" is ambiguous between "the recommendation is on this tab" and
// "there is no recommendation": the signal is suppressed exactly when it
// would disambiguate.
//
// findRecommendation resolves that without ever testing for absence, by
// ordering two presence checks. Every step asks whether something is there,
// which NCC can answer; no step asks whether something is missing, which it
// cannot.
func techDonate(ctx context.Context, rt *runtime.Ctx) error {
	if err := rt.NavigateTo(ctx, vision.ScreenAllianceTech); err != nil {
		return err
	}
	found, err := findRecommendation(ctx, rt)
	if err != nil {
		return err
	}
	if !found {
		return nil // nothing recommended right now
	}
	if _, err := rt.WaitFor(ctx, vision.ScreenAllianceTechDonate); err != nil {
		return err
	}
	// The donate button greys out rather than disappearing when the cap is
	// reached, so the anchor is cropped to include the counter that changes
	// with it — a structural discriminator, because NCC runs on intensity
	// and a pure desaturation leaves the correlation nearly untouched.
	_, err = tapUntilGone(ctx, rt, vision.ScreenAllianceTechDonate, "donate_button", maxDonations)
	return err
}

// findRecommendation opens the recommended tech's donate dialog, reporting
// whether there was one. It checks the tech list before the tab header:
// list-first costs one step in the common case where the recommendation is
// already on the selected tab, where tab-first costs two in every case.
func findRecommendation(ctx context.Context, rt *runtime.Ctx) (bool, error) {
	opened, err := tapIfPresent(ctx, rt, vision.ScreenAllianceTech, "tech_recommended_badge")
	if err != nil || opened {
		return opened, err
	}
	switched, err := tapIfPresent(ctx, rt, vision.ScreenAllianceTech, "tab_recommended_badge")
	if err != nil || !switched {
		return false, err
	}
	if err := rt.Sleep(ctx, tabSettle, 2*tabSettle); err != nil {
		return false, err
	}
	// Exactly two tabs, so exactly one retry. A loop here would ping-pong
	// forever on a UI change while reporting nothing wrong.
	return tapIfPresent(ctx, rt, vision.ScreenAllianceTech, "tech_recommended_badge")
}
