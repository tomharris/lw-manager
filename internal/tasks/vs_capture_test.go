package tasks

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// vs_capture's tests build their own small registry and graph, the same way
// roster_capture_test.go does: it needs a scrollable vs_ranking_weekly frame
// tall enough for vsListRegion/vsRowPitch, plus alliance_duel's
// ranking_button and vs_ranking_weekly's filter anchors, none of which the
// shared runtimetest grid was built to carry a scrollable region for.
const (
	vsFrameW = 160
	vsFrameH = 400
	vsPatchW = 24
	vsPatchH = 16
)

// Header anchor regions, all above vsListRegion's Y1 (0.185) so they never
// overlap the striped list band scrollCapture actually measures. Reused
// across screens with different seeds, the same trick roster_capture_test.go
// uses for its alliance/alliance_members header anchors: NCC discriminates on
// the pattern drawn, not on the region's coordinates.
var (
	vsHeaderLeftRegion  = transport.Rect{X1: 0.05, Y1: 0.02, X2: 0.35, Y2: 0.15}
	vsHeaderMidRegion   = transport.Rect{X1: 0.40, Y1: 0.02, X2: 0.65, Y2: 0.15}
	vsHeaderRightRegion = transport.Rect{X1: 0.70, Y1: 0.02, X2: 0.95, Y2: 0.15}
)

// Distinct seeds per anchor so their templates never coincidentally resemble
// one another.
const (
	seedAllianceDuelID       = 1
	seedRankingButton        = 2
	seedVSRankingID          = 3
	seedWeeklyTab            = 4
	seedVSRankingWeeklyID    = 5
	seedVSRankingAllianceBtn = 6
	seedYourAllianceChecked  = 7
	seedDuelProgressTab      = 8
)

// vsPatchTemplate is one anchor's deterministic pattern, matching
// rosterPatchTemplate's style in roster_capture_test.go.
func vsPatchTemplate(seed int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, vsPatchW, vsPatchH))
	for y := 0; y < vsPatchH; y++ {
		for x := 0; x < vsPatchW; x++ {
			v := uint8((x*29 + y*61 + seed*97) % 256)
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// vsStampPatch draws seed's pattern at region's rounded top-left corner —
// math.Round to match vision.Match's own rounding.
func vsStampPatch(img *image.RGBA, region transport.Rect, seed int) {
	x1 := int(math.Round(region.X1 * float64(vsFrameW)))
	y1 := int(math.Round(region.Y1 * float64(vsFrameH)))
	tpl := vsPatchTemplate(seed)
	draw.Draw(img, image.Rect(x1, y1, x1+vsPatchW, y1+vsPatchH), tpl, image.Point{}, draw.Src)
}

// vsStripePeriod is the vertical period of the synthetic list's pattern,
// matching scrollcapture_test.go's scrollStripePeriod: a shift within one
// period is measured directly, and a shift of exactly one period is
// indistinguishable from no movement.
const vsStripePeriod = 419

// vsBaseFrame is flat gray everywhere except vsListRegion, which carries
// shift-dependent striped content so vision.ScrollOffset can measure it —
// the same present-iff-drawn, static-elsewhere shape rosterBaseFrame uses,
// but shiftable so one frame builder can serve both the at-rest tests and
// the scrolling one.
//
// The row content used to be v = (yy/3*37) % 251 alone, the same degenerate
// linear-ramp-in-disguise pattern rosterBaseFrame and scrollFrame had (see
// rosterBaseFrame's comment for the mechanism and why it only became a
// problem once ScrollOffset started asking for a margin, not just a peak).
// Fixed the same way: a per-band marker column, keyed off the row band
// after the period wraparound so it stays consistent with the pattern it
// decorates.
func vsBaseFrame(shift int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, vsFrameW, vsFrameH))
	flat := color.RGBA{R: 100, G: 100, B: 100, A: 255}
	for y := 0; y < vsFrameH; y++ {
		for x := 0; x < vsFrameW; x++ {
			img.Set(x, y, flat)
		}
	}
	y0 := int(vsListRegion.Y1 * float64(vsFrameH))
	y1 := int(vsListRegion.Y2 * float64(vsFrameH))
	markerW := vsFrameW/7 + 1
	for y := y0; y < y1; y++ {
		yy := ((y+shift)%vsStripePeriod + vsStripePeriod) % vsStripePeriod
		band := yy / 3
		v := uint8((band * 37) % 200) // capped below 255 so the marker always stands out
		markerX := rand.New(rand.NewSource(int64(band))).Intn(vsFrameW)
		for x := 0; x < vsFrameW; x++ {
			px := v
			if x >= markerX && x < markerX+markerW {
				px = 255
			}
			img.Set(x, y, color.RGBA{R: px, G: px, B: uint8((x / 5 * 11) % 251), A: 255})
		}
	}
	return img
}

// allianceDuelFrame renders the alliance_duel screen: its identifying anchor
// always present, ranking_button only when showRanking — simulating the
// corpus's other tab mode, which carries no Ranking button at all —  and
// duel_progress_tab only when showProgressTab, reusing the header region
// vs_ranking_weekly's own your_alliance_checked anchor uses (NCC
// discriminates on the pattern, not the region's coordinates, so this is
// safe on a different screen). duel_progress_tab is cropped from its
// UNSELECTED state (commit 2d1d0ef), so "present" here means the tab has not
// been chosen yet — the exact condition under which ensureRankingButton
// should tap it.
func allianceDuelFrame(showRanking, showProgressTab bool) image.Image {
	img := vsBaseFrame(0)
	vsStampPatch(img, vsHeaderLeftRegion, seedAllianceDuelID)
	if showRanking {
		vsStampPatch(img, vsHeaderMidRegion, seedRankingButton)
	}
	if showProgressTab {
		vsStampPatch(img, vsHeaderRightRegion, seedDuelProgressTab)
	}
	return img
}

// vsRankingFrame renders the intermediate vs_ranking screen (its identifying
// anchor plus the weekly_tab the graph taps to leave it).
func vsRankingFrame() image.Image {
	img := vsBaseFrame(0)
	vsStampPatch(img, vsHeaderLeftRegion, seedVSRankingID)
	vsStampPatch(img, vsHeaderMidRegion, seedWeeklyTab)
	return img
}

// vsRankingWeeklyFrame renders vs_ranking_weekly at list offset shift, with
// the Your Alliance control and its checkmark drawn only when asked.
func vsRankingWeeklyFrame(shift int, showAllianceButton, showChecked bool) image.Image {
	img := vsBaseFrame(shift)
	vsStampPatch(img, vsHeaderLeftRegion, seedVSRankingWeeklyID)
	if showAllianceButton {
		vsStampPatch(img, vsHeaderMidRegion, seedVSRankingAllianceBtn)
	}
	if showChecked {
		vsStampPatch(img, vsHeaderRightRegion, seedYourAllianceChecked)
	}
	return img
}

func vsRegistry() *vision.Registry {
	return &vision.Registry{
		ReferenceHeight: vsFrameH,
		Screens: []vision.Screen{
			{
				Name: vision.ScreenAllianceDuel,
				Anchors: []vision.Anchor{
					{ID: "alliance_duel", Template: vsPatchTemplate(seedAllianceDuelID), Region: vsHeaderLeftRegion, Threshold: 0.9, IdentifiesScreen: true},
					{ID: "ranking_button", Template: vsPatchTemplate(seedRankingButton), Region: vsHeaderMidRegion, Threshold: 0.9, IdentifiesScreen: false},
					{ID: "duel_progress_tab", Template: vsPatchTemplate(seedDuelProgressTab), Region: vsHeaderRightRegion, Threshold: 0.9, IdentifiesScreen: false},
				},
			},
			{
				Name: vision.ScreenVSRanking,
				Anchors: []vision.Anchor{
					{ID: "vs_ranking", Template: vsPatchTemplate(seedVSRankingID), Region: vsHeaderLeftRegion, Threshold: 0.9, IdentifiesScreen: true},
					{ID: "weekly_tab", Template: vsPatchTemplate(seedWeeklyTab), Region: vsHeaderMidRegion, Threshold: 0.9, IdentifiesScreen: false},
				},
			},
			{
				Name: vision.ScreenVSRankingWeekly,
				Anchors: []vision.Anchor{
					{ID: "vs_ranking_weekly", Template: vsPatchTemplate(seedVSRankingWeeklyID), Region: vsHeaderLeftRegion, Threshold: 0.9, IdentifiesScreen: true},
					{ID: "vs_ranking_alliance_button", Template: vsPatchTemplate(seedVSRankingAllianceBtn), Region: vsHeaderMidRegion, Threshold: 0.9, IdentifiesScreen: false},
					{ID: "your_alliance_checked", Template: vsPatchTemplate(seedYourAllianceChecked), Region: vsHeaderRightRegion, Threshold: 0.9, IdentifiesScreen: false},
				},
			},
		},
	}
}

// vsGraph is the sub-tree of DefaultGraph this route walks: alliance_duel ->
// vs_ranking -> vs_ranking_weekly. Entry is alliance_duel so tests do not
// need to script base or the vs_button tap that lands there — vsCapture
// itself starts its walk from alliance_duel.
func vsGraph() *runtime.Graph {
	return &runtime.Graph{
		Entry: vision.ScreenAllianceDuel,
		Edges: []runtime.Edge{
			{From: vision.ScreenAllianceDuel, To: vision.ScreenVSRanking, Action: runtime.ActionTap, AnchorID: "ranking_button"},
			{From: vision.ScreenVSRanking, To: vision.ScreenAllianceDuel, Action: runtime.ActionBack},
			{From: vision.ScreenVSRanking, To: vision.ScreenVSRankingWeekly, Action: runtime.ActionTap, AnchorID: "weekly_tab"},
			{From: vision.ScreenVSRankingWeekly, To: vision.ScreenVSRanking, Action: runtime.ActionBack},
		},
	}
}

// newVSHarness wires a Ctx against vsRegistry()/vsGraph() and a fake capture
// recorder, modelled on roster_capture_test.go's newRosterHarness.
func newVSHarness(t *testing.T, frames []image.Image) (*runtime.Ctx, *transport.ReplayTransport, *fakeCaptureRecorder) {
	t.Helper()
	tr, err := transport.NewReplayTransportFromImages(frames...)
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeCaptureRecorder{}
	c, err := runtime.New(runtime.Options{
		Transport:       tr,
		Registry:        vsRegistry(),
		Graph:           vsGraph(),
		Kill:            &runtimetest.FakeKill{},
		Capture:         &fakeCapturer{},
		CaptureRecorder: rec,
		AccountID:       1,
		Rand:            rand.New(rand.NewSource(7)),
		PollInterval:    time.Millisecond,
		WaitTimeout:     50 * time.Millisecond,
		RestartTimeout:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, tr, rec
}

// vsHappyPathFrames scripts landing on vs_ranking_weekly with the filter off
// and then applied by exactly one tap: alliance_duel (ranking button
// present) -> vs_ranking -> vs_ranking_weekly -> Your Alliance read as
// unchecked, tapped once, then read as checked. It ends on a checked,
// at-rest (shift 0) frame, which ReplayTransport then holds for whatever the
// caller does next — including scrollCapture's own screenshots, which is
// exactly what a list that is already at the bottom looks like: a captured
// frame's pixels do not move.
//
//   - 1 frame: NavigateTo(alliance_duel)'s CurrentScreen (Entry already
//     matches, so this is the only read)
//   - 1 frame: ensureRankingButton's Sees check (present on the first look)
//   - 1 frame: NavigateTo(vs_ranking_weekly)'s initial CurrentScreen
//   - 1 frame: Tap(alliance_duel, ranking_button)'s verify
//   - 1 frame: WaitFor(vs_ranking) after the tap
//   - 1 frame: Tap(vs_ranking, weekly_tab)'s verify
//   - 1 frame: WaitFor(vs_ranking_weekly) after the tap
//   - 1 frame: NavigateTo's own re-plan-loop CurrentScreen that confirms
//     arrival (see roster_capture_test.go's threeGroupFrames doc comment for
//     the same "WaitFor, then a confirming CurrentScreen" pair)
//   - 1 frame: applyAllianceFilter's loop, attempt 0: Sees(your_alliance_checked),
//     not yet checked — the read that must happen before any tap
//   - 1 frame: applyAllianceFilter's loop, attempt 0: Sees(vs_ranking_alliance_button)
//   - 1 frame: applyAllianceFilter's loop, attempt 0: Tap(vs_ranking_alliance_button) verify
//   - 1 frame: applyAllianceFilter's loop, attempt 1: Sees(your_alliance_checked), checked
func vsHappyPathFrames() []image.Image {
	allianceDuel := allianceDuelFrame(true, false)
	ranking := vsRankingFrame()
	weeklyUnchecked := vsRankingWeeklyFrame(0, true, false)
	weeklyChecked := vsRankingWeeklyFrame(0, true, true)

	return []image.Image{
		allianceDuel,    // NavigateTo(alliance_duel) CurrentScreen
		allianceDuel,    // ensureRankingButton Sees
		allianceDuel,    // NavigateTo(vs_ranking_weekly) initial CurrentScreen
		allianceDuel,    // Tap(ranking_button) verify
		ranking,         // WaitFor(vs_ranking)
		ranking,         // Tap(weekly_tab) verify
		weeklyUnchecked, // WaitFor(vs_ranking_weekly)
		weeklyUnchecked, // confirming CurrentScreen
		weeklyUnchecked, // attempt 0: Sees(your_alliance_checked) -> false
		weeklyUnchecked, // attempt 0: Sees(vs_ranking_alliance_button) -> true
		weeklyUnchecked, // attempt 0: Tap(vs_ranking_alliance_button) verify
		weeklyChecked,   // attempt 1: Sees(your_alliance_checked) -> true, done
	}
}

// vsAlreadyFilteredFrames scripts the run 362 hardware scenario directly:
// the game persisted the Your Alliance filter from a previous session, so it
// is already checked the moment vs_ranking_weekly is reached. The correct
// behaviour is to read that and stop — zero taps against the filter control.
// The old, unconditional-tap applyAllianceFilter would instead tap it
// (turning it off, per the handset repro) before ever reading state, which
// this frame script cannot represent faithfully for the old call order
// (there is no "the game turned it off" simulation here — frames are
// scripted, not stateful) but does still starve it: the old code's first
// call is Sees(vs_ranking_alliance_button), which this script answers with
// the confirming-arrival frame, not a frame prepared for that read, and it
// proceeds to tap and mis-consume the rest of the list in a way that does
// not end in success. That is enough to prove this test is not vacuously
// true for both implementations; see the task report for the actual
// before/after run.
func vsAlreadyFilteredFrames() []image.Image {
	allianceDuel := allianceDuelFrame(true, false)
	ranking := vsRankingFrame()
	weeklyChecked := vsRankingWeeklyFrame(0, true, true)

	return []image.Image{
		allianceDuel,  // NavigateTo(alliance_duel) CurrentScreen
		allianceDuel,  // ensureRankingButton Sees
		allianceDuel,  // NavigateTo(vs_ranking_weekly) initial CurrentScreen
		allianceDuel,  // Tap(ranking_button) verify
		ranking,       // WaitFor(vs_ranking)
		ranking,       // Tap(weekly_tab) verify
		weeklyChecked, // WaitFor(vs_ranking_weekly)
		weeklyChecked, // confirming CurrentScreen
		weeklyChecked, // attempt 0: Sees(your_alliance_checked) -> true, done
	}
}

// The whole route succeeds end to end: navigation, the filter, and a list
// that proves its bottom immediately (the held-last frame never moves, so
// the third zero-offset swipe settles it) all in one call.
func TestVSCaptureCapturesTheFilteredWeeklyRankingAsOneCompleteRun(t *testing.T) {
	rt, tr, rec := newVSHarness(t, vsHappyPathFrames())

	fn, ok := Get("vs_capture")
	if !ok {
		t.Fatal("vs_capture is not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("vs_capture: %v", err)
	}

	// 2 navigation taps (ranking_button, weekly_tab) + 1 filter tap.
	if got := countTaps(tr); got != 3 {
		t.Errorf("got %d taps, want 3", got)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("got %d RecordCapture calls, want exactly 1", len(rec.calls))
	}
	call := rec.calls[0]
	if call.route != "vs_ranking" {
		t.Errorf("route = %q, want %q", call.route, "vs_ranking")
	}
	if !call.complete {
		t.Error("want complete = true — the list proved its bottom")
	}
}

// The regression test for run 362 on the handset: the game persists the
// Your Alliance filter across sessions, so the task can arrive on
// vs_ranking_weekly with the filter already applied. applyAllianceFilter
// must read that and return without ever tapping the control — the old
// unconditional-tap code tapped it regardless, which is exactly what turned
// an already-applied filter off on hardware.
func TestVSCaptureAppliesAnAlreadyCheckedFilterWithZeroTaps(t *testing.T) {
	rt, tr, rec := newVSHarness(t, vsAlreadyFilteredFrames())

	fn, ok := Get("vs_capture")
	if !ok {
		t.Fatal("vs_capture is not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("vs_capture: %v", err)
	}
	// 2 navigation taps only (ranking_button, weekly_tab) — the filter was
	// already applied, so the Your Alliance control itself must never be
	// tapped.
	if got := countTaps(tr); got != 2 {
		t.Errorf("got %d taps, want 2 — an already-applied filter must not be tapped", got)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("got %d RecordCapture calls, want exactly 1", len(rec.calls))
	}
}

// Without the filter the ranking lists both alliances, so every enemy row
// would be parsed and fail to match, flooding review with rows that are not
// ours. The checkmark is the proof the tap landed, not the tap itself. This
// exercises the full retry budget: applyAllianceFilter must read-then-tap
// filterTapAttempts times, and only then give up — it must not tap more
// than that, and it must not give up sooner.
func TestVSCaptureFailsWhenTheAllianceFilterNeverConfirms(t *testing.T) {
	allianceDuel := allianceDuelFrame(true, false)
	ranking := vsRankingFrame()
	weeklyUnchecked := vsRankingWeeklyFrame(0, true, false)

	frames := []image.Image{
		allianceDuel, allianceDuel, allianceDuel, allianceDuel,
		ranking, ranking,
		weeklyUnchecked, weeklyUnchecked, // WaitFor + confirming CurrentScreen
	}
	// filterTapAttempts iterations, each: Sees(checked) -> false,
	// Sees(button) -> true, Tap(button) verify. The checkmark never appears
	// no matter how many times the control is read and tapped.
	for i := 0; i < filterTapAttempts; i++ {
		frames = append(frames, weeklyUnchecked, weeklyUnchecked, weeklyUnchecked)
	}
	frames = append(frames, weeklyUnchecked) // final Sees(checked): still false

	rt, tr, rec := newVSHarness(t, frames)
	fn, ok := Get("vs_capture")
	if !ok {
		t.Fatal("vs_capture is not registered")
	}
	if err := fn(context.Background(), rt); !errors.Is(err, ErrFilterNotApplied) {
		t.Fatalf("got %v, want ErrFilterNotApplied", err)
	}
	// 2 navigation taps + exactly filterTapAttempts filter taps: the loop
	// must stop at the bound, not retry forever.
	if got, want := countTaps(tr), 2+filterTapAttempts; got != want {
		t.Errorf("got %d taps, want %d", got, want)
	}
	if len(rec.calls) != 0 {
		t.Errorf("got %d RecordCapture calls, want 0 — a failed filter must not reach the scroll or recordFrames", len(rec.calls))
	}
}

// The Your Alliance control itself is absent, not merely unconfirmed. This
// must fail the same way, and without ever tapping — invariant #3, no task
// acts without a matched anchor first.
func TestVSCaptureFailsWhenTheAllianceButtonControlIsAbsent(t *testing.T) {
	allianceDuel := allianceDuelFrame(true, false)
	ranking := vsRankingFrame()
	weeklyNoButton := vsRankingWeeklyFrame(0, false, false)

	frames := []image.Image{
		allianceDuel, allianceDuel, allianceDuel, allianceDuel,
		ranking, ranking,
		weeklyNoButton, weeklyNoButton, // WaitFor + confirming CurrentScreen
		weeklyNoButton, // Sees(your_alliance_checked): absent
		weeklyNoButton, // Sees(vs_ranking_alliance_button): absent
	}

	rt, tr, rec := newVSHarness(t, frames)
	fn, _ := Get("vs_capture")
	if err := fn(context.Background(), rt); !errors.Is(err, ErrFilterNotApplied) {
		t.Fatalf("got %v, want ErrFilterNotApplied", err)
	}
	// Only the 2 navigation taps — the filter control was never there to tap.
	if got := countTaps(tr); got != 2 {
		t.Errorf("got %d taps, want 2 — an absent control must never be tapped", got)
	}
	if len(rec.calls) != 0 {
		t.Errorf("got %d RecordCapture calls, want 0", len(rec.calls))
	}
}

// The corpus finding ensureRankingButton originally existed to handle:
// Alliance Duel can land on a tab with no Ranking button in its bottom bar
// at all. With no Duel Progress tab either, there is nothing safe to tap
// toward a known state, so this must fail safely — named, and without ever
// tapping toward a guess — rather than surface as a generic anchor-not-found
// or, worse, act blindly.
func TestVSCaptureFailsWhenAllianceDuelHasNoRankingButtonOrProgressTab(t *testing.T) {
	// Held static: the same absent-everything frame answers every
	// ensureRankingButton retry and the subsequent duel_progress_tab check,
	// proving the bound is real rather than eventually finding a frame that
	// happens to have the button.
	frames := []image.Image{allianceDuelFrame(false, false)}

	rt, tr, rec := newVSHarness(t, frames)
	fn, _ := Get("vs_capture")
	err := fn(context.Background(), rt)
	if !errors.Is(err, ErrRankingTabUnavailable) {
		t.Fatalf("got %v, want ErrRankingTabUnavailable", err)
	}
	if got := countTaps(tr); got != 0 {
		t.Errorf("got %d taps, want 0 — a missing Ranking button must never be tapped around", got)
	}
	if len(rec.calls) != 0 {
		t.Errorf("got %d RecordCapture calls, want 0", len(rec.calls))
	}
}

// duel_progress_tab was cropped from that tab's UNSELECTED state, so it
// matching some frames but Ranking still never appearing is a real possible
// outcome, not just "hasn't rendered yet" — the one anchored recovery was
// tried and did not work. This must still fail with ErrRankingTabUnavailable,
// having tapped exactly once (the tab itself), and never guess among the
// other unlabelled tabs.
func TestVSCaptureFailsWhenDuelProgressTabDoesNotBringRankingBack(t *testing.T) {
	noButtonWithTab := allianceDuelFrame(false, true)

	frames := []image.Image{
		noButtonWithTab, // NavigateTo(alliance_duel) CurrentScreen
		noButtonWithTab, // ensureRankingButton attempt 0: absent
		noButtonWithTab, // ensureRankingButton attempt 1: absent
		noButtonWithTab, // ensureRankingButton attempt 2: absent
		noButtonWithTab, // Sees(duel_progress_tab): present
		noButtonWithTab, // Tap(duel_progress_tab) verify
		noButtonWithTab, // re-check Sees(ranking_button): still absent
	}

	rt, tr, rec := newVSHarness(t, frames)
	fn, _ := Get("vs_capture")
	err := fn(context.Background(), rt)
	if !errors.Is(err, ErrRankingTabUnavailable) {
		t.Fatalf("got %v, want ErrRankingTabUnavailable", err)
	}
	if got := countTaps(tr); got != 1 {
		t.Errorf("got %d taps, want 1 — only the anchored Duel Progress tap, nothing else", got)
	}
	if len(rec.calls) != 0 {
		t.Errorf("got %d RecordCapture calls, want 0", len(rec.calls))
	}
}

// A landing animation that has not finished drawing the bottom bar yet is
// not the same defect as a genuinely wrong tab: the retry must be able to
// recover once the button actually renders, and then carry on to a normal
// capture.
func TestVSCaptureRecoversWhenTheRankingButtonAppearsAfterASettle(t *testing.T) {
	allianceDuelAbsent := allianceDuelFrame(false, false)
	allianceDuelPresent := allianceDuelFrame(true, false)
	ranking := vsRankingFrame()
	weeklyUnchecked := vsRankingWeeklyFrame(0, true, false)
	weeklyChecked := vsRankingWeeklyFrame(0, true, true)

	frames := []image.Image{
		allianceDuelAbsent,  // NavigateTo(alliance_duel) CurrentScreen
		allianceDuelAbsent,  // ensureRankingButton attempt 0: absent
		allianceDuelPresent, // ensureRankingButton attempt 1: present
		allianceDuelPresent, // NavigateTo(vs_ranking_weekly) initial CurrentScreen
		allianceDuelPresent, // Tap(ranking_button) verify
		ranking,             // WaitFor(vs_ranking)
		ranking,             // Tap(weekly_tab) verify
		weeklyUnchecked,     // WaitFor(vs_ranking_weekly)
		weeklyUnchecked,     // confirming CurrentScreen
		weeklyUnchecked,     // attempt 0: Sees(your_alliance_checked) -> false
		weeklyUnchecked,     // attempt 0: Sees(vs_ranking_alliance_button) -> true
		weeklyUnchecked,     // attempt 0: Tap(vs_ranking_alliance_button) verify
		weeklyChecked,       // attempt 1: Sees(your_alliance_checked) -> true, done
	}

	rt, _, rec := newVSHarness(t, frames)
	fn, _ := Get("vs_capture")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("vs_capture: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("got %d RecordCapture calls, want exactly 1", len(rec.calls))
	}
	if !rec.calls[0].complete {
		t.Error("want complete = true")
	}
}

// ensureRankingButton's settle retries are exhausted (ranking_button never
// appears on alliance_duel), but duel_progress_tab is present, so it is
// tapped, and after that single tap ranking_button does appear — the tab was
// genuinely unselected and this is the exact case the anchor exists for. The
// route then carries on through a normal capture.
func TestVSCaptureSelectsDuelProgressTabWhenRankingButtonIsAbsent(t *testing.T) {
	noButtonWithTab := allianceDuelFrame(false, true)
	withButton := allianceDuelFrame(true, false)
	ranking := vsRankingFrame()
	weeklyChecked := vsRankingWeeklyFrame(0, true, true)

	frames := []image.Image{
		noButtonWithTab, // NavigateTo(alliance_duel) CurrentScreen
		noButtonWithTab, // ensureRankingButton attempt 0: absent
		noButtonWithTab, // ensureRankingButton attempt 1: absent
		noButtonWithTab, // ensureRankingButton attempt 2: absent
		noButtonWithTab, // Sees(duel_progress_tab): present
		noButtonWithTab, // Tap(duel_progress_tab) verify
		withButton,      // re-check Sees(ranking_button): now present
		withButton,      // NavigateTo(vs_ranking_weekly) initial CurrentScreen
		withButton,      // Tap(ranking_button) verify
		ranking,         // WaitFor(vs_ranking)
		ranking,         // Tap(weekly_tab) verify
		weeklyChecked,   // WaitFor(vs_ranking_weekly)
		weeklyChecked,   // confirming CurrentScreen
		weeklyChecked,   // attempt 0: Sees(your_alliance_checked) -> true, done
	}

	rt, tr, rec := newVSHarness(t, frames)
	fn, _ := Get("vs_capture")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("vs_capture: %v", err)
	}
	// duel_progress_tab + ranking_button + weekly_tab: the filter was
	// already checked, so applyAllianceFilter contributes no tap here.
	if got := countTaps(tr); got != 3 {
		t.Errorf("got %d taps, want 3 (duel_progress_tab, ranking_button, weekly_tab)", got)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("got %d RecordCapture calls, want exactly 1", len(rec.calls))
	}
}

// vsExpandScrollScript turns a sequence of per-swipe pixel shifts into the
// exact frame sequence scrollCapture will pull from the transport once
// landed on a checked vs_ranking_weekly, mirroring
// scrollcapture_test.go's expandScrollScript: one frame-0 raw screenshot,
// then per swipe attempt, rt.Swipe's own pre-swipe verify (any recognized
// frame — the prior content frame serves) followed by the post-swipe frame
// that is measured and, if kept, stored.
func vsExpandScrollScript(shifts []int) []image.Image {
	pos := 0
	cur := vsRankingWeeklyFrame(pos, true, true)
	imgs := []image.Image{cur}

	for i, s := range shifts {
		if i >= maxScrollFrames-1 {
			break
		}
		prev := cur
		pos += s
		cur = vsRankingWeeklyFrame(pos, true, true)
		imgs = append(imgs, prev)
		imgs = append(imgs, cur)
	}
	return imgs
}

// The bottom is never proven — the list keeps moving right up to
// maxScrollFrames. The capture must still be persisted, and marked not
// complete, because ingest reads absence as a zero score only on a capture
// proven to have reached the bottom.
func TestVSCaptureRecordsAnIncompleteScrollAsPartial(t *testing.T) {
	shifts := make([]int, maxScrollFrames+10)
	for i := range shifts {
		shifts[i] = 40 // matches scrollcapture_test.go's own overshoot-free step
	}

	// vsHappyPathFrames lands the filter-applied route and answers
	// applyAllianceFilter's own Sees(your_alliance_checked) with its final
	// frame; vsExpandScrollScript then starts fresh with scrollCapture's own
	// frame-0 raw screenshot at the same, checked, shift-0 position — a
	// distinct consumption from the ReplayTransport's queue, not a reuse of
	// the same slot.
	frames := append(vsHappyPathFrames(), vsExpandScrollScript(shifts)...)

	rt, _, rec := newVSHarness(t, frames)
	fn, _ := Get("vs_capture")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("vs_capture: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("got %d RecordCapture calls, want exactly 1", len(rec.calls))
	}
	call := rec.calls[0]
	if call.route != "vs_ranking" {
		t.Errorf("route = %q, want %q", call.route, "vs_ranking")
	}
	if call.complete {
		t.Error("want complete = false — the bottom was never proven")
	}
	if len(call.frames) == 0 {
		t.Error("want at least one frame recorded — a partial capture's frames are still evidence")
	}
}

// This is the branch with the sharpest consequence: the weekly ranking lists
// only members with a nonzero score (94 ranked rows against 96 members), so
// ingest reads an absent member as a zero only on a capture proven to have
// reached the bottom. A genuine mid-scroll error — not just running out of
// frame budget — must still persist what was captured before it, mark that
// capture not complete, and surface the failure to the scheduler rather than
// swallowing it. Losing the frames here would discard evidence outright;
// recording them as complete would silently zero out members who were simply
// never photographed.
//
// The shift of 180px is measured, not guessed: vsListRegion/vsRowPitch give
// a usable height of 118px at vsFrameH (246px of region less one 128px row),
// so a single swipe reporting 180px of travel is unambiguously further than
// the visible region could have shown — vision.ScrollOffset confirms it
// measures cleanly at exactly 180 for this shift, rather than wrapping into
// something smaller via vsStripePeriod, the way a larger shift can (measured
// by sweeping shift values against vision.ScrollOffset directly).
func TestVSCaptureRecordsThePartialScrollAndReturnsTheErrorOnAGenuineScrollFailure(t *testing.T) {
	frames := append(vsHappyPathFrames(), vsExpandScrollScript([]int{180})...)

	rt, _, rec := newVSHarness(t, frames)
	fn, _ := Get("vs_capture")
	err := fn(context.Background(), rt)

	if !errors.Is(err, ErrScrollOvershot) {
		t.Fatalf("got %v, want it to wrap ErrScrollOvershot — a genuine scroll failure must be visible to the caller, not swallowed", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("got %d RecordCapture calls, want exactly 1 — the frames collected before the failure must still be persisted", len(rec.calls))
	}
	call := rec.calls[0]
	if call.route != "vs_ranking" {
		t.Errorf("route = %q, want %q", call.route, "vs_ranking")
	}
	if call.complete {
		t.Error("want complete = false — a mid-scroll failure must never be recorded as a proven bottom")
	}
	if len(call.frames) == 0 {
		t.Error("want at least one frame recorded — the frame-0 capture taken before the overshot swipe is still evidence and must not be discarded")
	}
}
