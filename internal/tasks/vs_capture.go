package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrFilterNotApplied reports that the Your Alliance control was tapped and
// the green checkmark never appeared. Without the filter the ranking lists
// both alliances, so every enemy row would be parsed, fail to match, and
// flood review with rows that were never ours — the checkmark is the only
// local proof the tap actually applied the filter, not just that Tap
// returned nil.
var ErrFilterNotApplied = errors.New("tasks: the Your Alliance filter did not apply")

// ErrRankingTabUnavailable reports that Ranking was absent on Alliance Duel,
// and tapping toward Duel Progress — the one anchored recovery available —
// did not bring it back either.
//
// Alliance Duel has more than one tab mode: a corpus frame caught it showing
// an "Infinite Roulette" / "Duel League" strip instead of the one that
// carries Ranking, with no Ranking button on screen at all. duel_progress_tab
// (cropped on hardware in commit 2d1d0ef, from its deliberately UNSELECTED
// state — NCC scores an inverted template near -1, so one crop cannot serve
// both states) is the only anchor the manifest carries for steering toward a
// known tab, and ensureRankingButton taps it when present. This sentinel
// covers both ways that recovery can fail to leave a known-good screen: the
// tab anchor was never there to tap either, or it was tapped and Ranking
// still never appeared. Either way there is nothing else on the screen safe
// to try — the other tabs remain unlabelled, and guessing among them is
// exactly the blind tap invariant #3 forbids.
var ErrRankingTabUnavailable = errors.New("tasks: alliance duel has no Ranking button on its current tab")

// vsListRegion is the ranking's scrollable area: below the column header and
// above the pinned self row. The pinned row is excluded deliberately — it
// sits outside the scroll region and also appears in its natural position in
// the list, so including it would photograph one member twice at two
// different screen positions.
var vsListRegion = transport.Rect{X1: 0.03, Y1: 0.185, X2: 0.97, Y2: 0.80}

// vsRowPitch is the ranking row height measured on the handset. It differs
// from the roster's, which is why pitch is a per-list parameter.
const vsRowPitch = 128

// rankingButtonCheckAttempts bounds the wait for the Ranking button after
// landing on Alliance Duel. Three, matching chevronTapAttempts and
// executeTapAttempts.
const rankingButtonCheckAttempts = 3

// filterTapAttempts bounds applyAllianceFilter's read-tap loop. Three,
// matching chevronTapAttempts and executeTapAttempts.
const filterTapAttempts = 3

// rankingButtonSettle paces the retries above. It only ever covers a landing
// animation that has not finished drawing the bottom bar yet — it cannot fix
// a screen that is genuinely on a different tab, which is why the loop still
// ends in ErrRankingTabUnavailable rather than retrying forever. A var only
// so the device-free suite can collapse it; see TestMain.
var rankingButtonSettle = 900 * time.Millisecond

func init() { Register("vs_capture", vsCapture) }

// vsCapture walks base -> alliance_duel -> ranking -> weekly -> your
// alliance and captures the whole list.
//
// Weekly and Daily have different layouts — Daily carries a weekday tab
// strip that Weekly does not, so the list starts higher on Weekly — and this
// route commits to Weekly.
//
// The filtered and unfiltered weekly views are one screen, vs_ranking_weekly,
// not two: they differ only by whether a checkbox is ticked, and template
// matching cannot express the absence of something. The filter is confirmed
// by querying for the your_alliance_checked anchor after tapping, not by the
// screen changing.
func vsCapture(ctx context.Context, rt *runtime.Ctx) error {
	if err := rt.NavigateTo(ctx, vision.ScreenAllianceDuel); err != nil {
		return fmt.Errorf("tasks: navigating to alliance duel: %w", err)
	}
	if err := ensureRankingButton(ctx, rt); err != nil {
		return err
	}
	if err := rt.NavigateTo(ctx, vision.ScreenVSRankingWeekly); err != nil {
		return fmt.Errorf("tasks: navigating to the weekly ranking: %w", err)
	}
	if err := applyAllianceFilter(ctx, rt); err != nil {
		return err
	}

	spec := ScrollSpec{
		Screen: vision.ScreenVSRankingWeekly,
		Region: vsListRegion,
		Pitch:  vsRowPitch,
		// SwipeFrac: measured on the handset at 0.35 (the roster's own
		// value), a swipe here produced 665px of travel against probe 1's
		// 748px limit — inside, but with little headroom, and fling
		// varies. 0.25 pulls that back without disturbing the roster's own
		// already-working 0.35 (ScrollSpec's doc comment on SwipeFrac has
		// the full comparison).
		SwipeFrac: 0.25,
	}
	frames, complete, err := scrollCapture(ctx, rt, spec)
	if err != nil {
		// A capture that failed mid-scroll is still worth persisting: its
		// frames are evidence, and marking it partial is what stops ingest
		// reading absence as a zero.
		if rerr := recordFrames(ctx, rt, "vs_ranking", frames, false); rerr != nil {
			return fmt.Errorf("tasks: capturing the ranking (%v) and recording it: %w", err, rerr)
		}
		return fmt.Errorf("tasks: capturing the ranking: %w", err)
	}
	return recordFrames(ctx, rt, "vs_ranking", frames, complete)
}

// ensureRankingButton confirms Alliance Duel is on the tab that carries the
// Ranking button before anything downstream assumes it is there.
//
// The corpus found a frame where it is not: Alliance Duel has more than one
// tab mode, and one of them ("Infinite Roulette" / "Duel League") carries no
// Ranking button at all. The bounded settle retry below exists solely to
// cover a landing animation still drawing the bottom bar; it does not touch
// the screen, only reads it.
//
// If the button is still absent after that, this reaches for the one
// anchored recovery the manifest carries: duel_progress_tab, cropped from
// its deliberately UNSELECTED state, so its presence means the tab has not
// been chosen yet — exactly the condition under which tapping it is safe and
// useful (see the doc comment on ErrRankingTabUnavailable for why a crop of
// the selected state could not do this job). If that anchor is not there
// either, or tapping it still does not produce a Ranking button, this
// returns a sentinel that says so, rather than letting a bare NavigateTo
// fail deep inside a Tap with the generic ErrAnchorNotFound. There is still
// nothing else on this screen safe to try: the other tabs remain unlabelled,
// and guessing among them is exactly the blind tap invariant #3 forbids.
func ensureRankingButton(ctx context.Context, rt *runtime.Ctx) error {
	for i := 0; i < rankingButtonCheckAttempts; i++ {
		present, err := rt.Sees(ctx, vision.ScreenAllianceDuel, "ranking_button")
		if err != nil {
			return fmt.Errorf("tasks: looking for the Ranking button: %w", err)
		}
		if present {
			return nil
		}
		if i < rankingButtonCheckAttempts-1 {
			if err := rt.Sleep(ctx, rankingButtonSettle, 2*rankingButtonSettle); err != nil {
				return err
			}
		}
	}

	onProgressTab, err := rt.Sees(ctx, vision.ScreenAllianceDuel, "duel_progress_tab")
	if err != nil {
		return fmt.Errorf("tasks: looking for the Duel Progress tab: %w", err)
	}
	if !onProgressTab {
		return fmt.Errorf("tasks: no Ranking button after %d checks and no Duel Progress tab to select, probably on a different Alliance Duel tab: %w",
			rankingButtonCheckAttempts, ErrRankingTabUnavailable)
	}
	if err := rt.Tap(ctx, vision.ScreenAllianceDuel, "duel_progress_tab"); err != nil {
		return fmt.Errorf("tasks: tapping the Duel Progress tab: %w", err)
	}
	if err := rt.Sleep(ctx, rankingButtonSettle, 2*rankingButtonSettle); err != nil {
		return err
	}
	present, err := rt.Sees(ctx, vision.ScreenAllianceDuel, "ranking_button")
	if err != nil {
		return fmt.Errorf("tasks: looking for the Ranking button: %w", err)
	}
	if present {
		return nil
	}
	return fmt.Errorf("tasks: no Ranking button after selecting Duel Progress, probably on a different Alliance Duel tab: %w",
		ErrRankingTabUnavailable)
}

// applyAllianceFilter checks Your Alliance and confirms the checkmark, which
// is the only local proof the tap was taken. Without the filter the list
// carries both alliances and every enemy row would fail to match, flooding
// review with rows that were never ours.
//
// The game persists this filter across sessions, so the task can just as
// easily arrive with it already applied as with it off — run 362 on the
// handset hit exactly that, and the original version of this function
// tapped the control unconditionally before ever reading its state. Against
// an already-applied filter that inverted it: the tap turned the checkmark
// off, and the confirmation logic then correctly (and unhelpfully) reported
// that the checkmark was missing. No device-free test could have caught
// that, because a fake always starts from whatever state the test author
// imagined, never from "the game silently remembered last time."
//
// The fix is a loop that reads before every tap, not just before the first
// one:
//
//   - Idempotent (invariant #2): re-running against an already-filtered
//     screen taps nothing and returns immediately, rather than toggling the
//     filter off.
//   - Self-correcting: because state is re-read at the top of every
//     iteration rather than assumed from the tap that was just issued, a
//     retry can never double-tap a checkbox that did register but rendered
//     its checkmark slowly. A loop that instead tapped first and only
//     re-read after would flip an already-applied filter right back off —
//     the same bug this function exists to fix, wearing a retry-count
//     disguise.
func applyAllianceFilter(ctx context.Context, rt *runtime.Ctx) error {
	for attempt := 0; attempt < filterTapAttempts; attempt++ {
		checked, err := rt.Sees(ctx, vision.ScreenVSRankingWeekly, "your_alliance_checked")
		if err != nil {
			return fmt.Errorf("tasks: confirming the alliance filter: %w", err)
		}
		if checked {
			return nil
		}
		on, err := rt.Sees(ctx, vision.ScreenVSRankingWeekly, "vs_ranking_alliance_button")
		if err != nil {
			return fmt.Errorf("tasks: looking for the Your Alliance control: %w", err)
		}
		if !on {
			return fmt.Errorf("tasks: the Your Alliance control is not on screen: %w", ErrFilterNotApplied)
		}
		if err := rt.Tap(ctx, vision.ScreenVSRankingWeekly, "vs_ranking_alliance_button"); err != nil {
			return fmt.Errorf("tasks: tapping Your Alliance: %w", err)
		}
		if err := rt.Sleep(ctx, swipeSettleMin, swipeSettleMax); err != nil {
			return err
		}
	}
	checked, err := rt.Sees(ctx, vision.ScreenVSRankingWeekly, "your_alliance_checked")
	if err != nil {
		return fmt.Errorf("tasks: confirming the alliance filter: %w", err)
	}
	if !checked {
		return fmt.Errorf("tasks: your_alliance_checked did not appear after tapping Your Alliance: %w", ErrFilterNotApplied)
	}
	return nil
}
