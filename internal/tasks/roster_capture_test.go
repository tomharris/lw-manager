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

// roster_capture's tests build their own small registry, the same way
// scrollcapture_test.go does: it needs a tall, scrollable frame plus two
// extra toggle anchors (chevron_collapsed/chevron_expanded) the shared
// runtimetest grid was never built to carry, and a second screen
// (alliance) to navigate from.
const (
	rosterFrameW = 160
	rosterFrameH = 400
	patchW       = 24
	patchH       = 16
)

// Header anchor regions, all above memberListRegion's Y1 (0.42) so they
// never overlap the striped list band scrollCapture actually measures.
var (
	allianceIDRegion        = transport.Rect{X1: 0.05, Y1: 0.03, X2: 0.35, Y2: 0.18}
	membersButtonRegion     = transport.Rect{X1: 0.45, Y1: 0.03, X2: 0.75, Y2: 0.18}
	allianceMembersIDRegion = transport.Rect{X1: 0.05, Y1: 0.03, X2: 0.35, Y2: 0.18}
	chevronCollapsedRegion  = transport.Rect{X1: 0.45, Y1: 0.03, X2: 0.65, Y2: 0.18}
	chevronExpandedRegion   = transport.Rect{X1: 0.75, Y1: 0.03, X2: 0.95, Y2: 0.18}
)

// Distinct seeds per anchor so their templates never coincidentally
// resemble one another.
const (
	seedAllianceID        = 1
	seedMembersButton     = 2
	seedAllianceMembersID = 3
	seedChevronCollapsed  = 4
	seedChevronExpanded   = 5
)

// rosterPatchTemplate is one anchor's deterministic pattern, matching
// scrollAnchorTemplate's style in scrollcapture_test.go.
func rosterPatchTemplate(seed int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, patchW, patchH))
	for y := 0; y < patchH; y++ {
		for x := 0; x < patchW; x++ {
			v := uint8((x*29 + y*61 + seed*97) % 256)
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// stampPatch draws seed's pattern at region's rounded top-left corner —
// math.Round to match vision.Match's own rounding (see scrollcapture_test.go's
// drawScrollAnchor comment on why truncating here would knock the score out
// of range).
func stampPatch(img *image.RGBA, region transport.Rect, seed int) {
	x1 := int(math.Round(region.X1 * float64(rosterFrameW)))
	y1 := int(math.Round(region.Y1 * float64(rosterFrameH)))
	tpl := rosterPatchTemplate(seed)
	draw.Draw(img, image.Rect(x1, y1, x1+patchW, y1+patchH), tpl, image.Point{}, draw.Src)
}

// rosterBaseFrame is flat gray everywhere except memberListRegion, which
// carries real row-varying content. Flat everywhere else means an unstamped
// header anchor scores near zero against its template — the same
// present-iff-drawn property runtimetest's grid gives every cell — rather
// than however it happens to correlate with striped noise.
func rosterBaseFrame() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, rosterFrameW, rosterFrameH))
	flat := color.RGBA{R: 100, G: 100, B: 100, A: 255}
	for y := 0; y < rosterFrameH; y++ {
		for x := 0; x < rosterFrameW; x++ {
			img.Set(x, y, flat)
		}
	}
	y0 := int(memberListRegion.Y1 * float64(rosterFrameH))
	y1 := int(memberListRegion.Y2 * float64(rosterFrameH))
	for y := y0; y < y1; y++ {
		v := uint8((y / 3 * 37) % 251)
		for x := 0; x < rosterFrameW; x++ {
			img.Set(x, y, color.RGBA{R: v, G: v, B: uint8((x / 5 * 11) % 251), A: 255})
		}
	}
	return img
}

// allianceScreenFrame renders the alliance screen: its identifying anchor
// plus the members_button the graph taps to leave it.
func allianceScreenFrame() image.Image {
	img := rosterBaseFrame()
	stampPatch(img, allianceIDRegion, seedAllianceID)
	stampPatch(img, membersButtonRegion, seedMembersButton)
	return img
}

// rosterFrame renders the alliance_members screen with its identifying
// anchor always present and the two chevrons drawn only when asked —
// exactly the "a button that is not rendered is a button the matcher
// cannot find" property runtimetest.FrameWithout documents.
func rosterFrame(collapsed, expanded bool) image.Image {
	img := rosterBaseFrame()
	stampPatch(img, allianceMembersIDRegion, seedAllianceMembersID)
	if collapsed {
		stampPatch(img, chevronCollapsedRegion, seedChevronCollapsed)
	}
	if expanded {
		stampPatch(img, chevronExpandedRegion, seedChevronExpanded)
	}
	return img
}

func rosterRegistry() *vision.Registry {
	return &vision.Registry{
		ReferenceHeight: rosterFrameH,
		Screens: []vision.Screen{
			{
				Name: vision.ScreenAlliance,
				Anchors: []vision.Anchor{
					{ID: "alliance", Template: rosterPatchTemplate(seedAllianceID), Region: allianceIDRegion, Threshold: 0.9, IdentifiesScreen: true},
					{ID: "members_button", Template: rosterPatchTemplate(seedMembersButton), Region: membersButtonRegion, Threshold: 0.9, IdentifiesScreen: false},
				},
			},
			{
				Name: vision.ScreenAllianceMembers,
				Anchors: []vision.Anchor{
					{ID: "alliance_members", Template: rosterPatchTemplate(seedAllianceMembersID), Region: allianceMembersIDRegion, Threshold: 0.9, IdentifiesScreen: true},
					{ID: "chevron_collapsed", Template: rosterPatchTemplate(seedChevronCollapsed), Region: chevronCollapsedRegion, Threshold: 0.9, IdentifiesScreen: false},
					{ID: "chevron_expanded", Template: rosterPatchTemplate(seedChevronExpanded), Region: chevronExpandedRegion, Threshold: 0.9, IdentifiesScreen: false},
				},
			},
		},
	}
}

// fakeCaptureRecorder fakes runtime.CaptureRecorder, the interface this task
// added ahead of Task 10's database-backed implementation. It records every
// call verbatim so a test can assert both the call count (one per run) and
// the renumbered Seq values.
type fakeCaptureRecorder struct {
	calls []recordedCapture
}

type recordedCapture struct {
	accountID int64
	route     string
	frames    []runtime.CaptureFrameRef
	complete  bool
}

func (f *fakeCaptureRecorder) RecordCapture(ctx context.Context, accountID int64, route string, frames []runtime.CaptureFrameRef, complete bool) error {
	cp := append([]runtime.CaptureFrameRef(nil), frames...)
	f.calls = append(f.calls, recordedCapture{accountID: accountID, route: route, frames: cp, complete: complete})
	return nil
}

// newRosterHarness wires a Ctx against rosterRegistry() and a minimal
// alliance <-> alliance_members graph, modelled on scrollcapture_test.go's
// newScrollHarness.
func newRosterHarness(t *testing.T, frames []image.Image) (*runtime.Ctx, *transport.ReplayTransport, *fakeCapturer, *fakeCaptureRecorder) {
	t.Helper()
	tr, err := transport.NewReplayTransportFromImages(frames...)
	if err != nil {
		t.Fatal(err)
	}
	cap := &fakeCapturer{}
	rec := &fakeCaptureRecorder{}
	c, err := runtime.New(runtime.Options{
		Transport: tr,
		Registry:  rosterRegistry(),
		Graph: &runtime.Graph{
			Entry: vision.ScreenAlliance,
			Edges: []runtime.Edge{
				{From: vision.ScreenAlliance, To: vision.ScreenAllianceMembers, Action: runtime.ActionTap, AnchorID: "members_button"},
				{From: vision.ScreenAllianceMembers, To: vision.ScreenAlliance, Action: runtime.ActionBack},
			},
		},
		Kill:            &runtimetest.FakeKill{},
		Capture:         cap,
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
	return c, tr, cap, rec
}

// threeGroupFrames scripts the exact frame sequence roster_capture pulls
// from the transport when three rank groups are visited in turn and each
// group's list proves its bottom on the first swipe streak:
//
//   - 4 alliance frames for the NavigateTo(alliance)/Capture/
//     NavigateTo(alliance_members) sequence, then 2 alliance_members frames
//     for NavigateTo(alliance_members)'s own two reads: WaitFor's arrival,
//     and the confirming CurrentScreen its outer replan loop takes on its
//     next pass before returning — see radar_test.go's toRadar, which
//     documents the same "WaitFor, arrival CurrentScreen" pair.
//   - per group: 1 frame for the loop's own chevron_collapsed check, 2 for
//     openGroup (the tap's own verify, then the confirm), 7 for
//     scrollCapture (1 raw frame-0 capture + 3 swipes x (Swipe's pre-verify
//   - the post-swipe measurement) — all held static so every offset
//     measures zero and the third zero proves the bottom), and 2 for
//     closeGroup — always confirmed by "collapsed", since the group this
//     task just closed is genuinely collapsed again regardless of whether
//     any other group remains; "no groups left" is a separate signal.
//   - 1 final frame for the loop's own next chevron_collapsed check, which
//     is the one that finds nothing left and breaks.
func threeGroupFrames() []image.Image {
	alliance := allianceScreenFrame()
	collapsed := rosterFrame(true, false)
	open := rosterFrame(false, true)
	done := rosterFrame(false, false)

	frames := []image.Image{alliance, alliance, alliance, alliance, collapsed, collapsed}
	for i := 0; i < 3; i++ {
		frames = append(frames, collapsed)       // loop's chevron_collapsed check
		frames = append(frames, collapsed, open) // openGroup: tap verify, then confirm
		frames = append(frames, rep(open, 7)...) // scrollCapture
		frames = append(frames, open, collapsed) // closeGroup: tap verify, then confirm
	}
	frames = append(frames, done) // final loop check: no collapsed chevron remains
	return frames
}

// The loop visits every collapsed group and stops once none remain.
func TestRosterCaptureVisitsEveryGroupThenStops(t *testing.T) {
	rt, tr, _, _ := newRosterHarness(t, threeGroupFrames())

	fn, ok := Get("roster_capture")
	if !ok {
		t.Fatal("roster_capture not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("roster_capture: %v", err)
	}
	if got := countKind(tr, "swipe"); got != 9 {
		t.Errorf("got %d swipes, want 9 (3 groups x 3 swipes each)", got)
	}
	// Navigation's members_button tap, plus open+close per group.
	if got := countTaps(tr); got != 7 {
		t.Errorf("got %d taps, want 7 (1 navigation + 2 per group x 3 groups)", got)
	}
}

// One capture per run: recordFrames is called exactly once with the alliance
// frame plus every group's frames concatenated, and Seq renumbered
// sequentially across the whole run, since scrollCapture numbers each call
// from zero and capture_frames has UNIQUE (capture_id, seq).
func TestRosterCaptureRecordsOneCaptureWithRenumberedSeq(t *testing.T) {
	rt, _, _, rec := newRosterHarness(t, threeGroupFrames())

	fn, ok := Get("roster_capture")
	if !ok {
		t.Fatal("roster_capture not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("roster_capture: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("got %d RecordCapture calls, want exactly 1", len(rec.calls))
	}
	call := rec.calls[0]
	if call.route != "roster" {
		t.Errorf("route = %q, want %q", call.route, "roster")
	}
	if !call.complete {
		t.Error("want complete = true — every group proved its bottom")
	}
	if len(call.frames) != 4 {
		t.Fatalf("got %d frames, want 4 (the alliance frame, then one per group)", len(call.frames))
	}
	for i, f := range call.frames {
		if f.Seq != i {
			t.Errorf("frame %d has Seq %d, want %d — Seq must be renumbered across the whole run, not left at scrollCapture's own per-call zero", i, f.Seq, i)
		}
	}
	if call.frames[0].GroupKey != vision.AllianceSummaryGroupKey {
		t.Errorf("frame 0 GroupKey = %q, want %q — the alliance frame must be recorded first and tagged so ingest can find it and skip segmenting it", call.frames[0].GroupKey, vision.AllianceSummaryGroupKey)
	}
	for i, f := range call.frames[1:] {
		if f.GroupKey != "" {
			t.Errorf("group frame %d GroupKey = %q, want empty — rank is read from each frame's own sticky header at ingest, not asserted here", i, f.GroupKey)
		}
	}
}

// navToChevronCheckFrames is the shared prefix every test below needs before
// the group loop's own chevron_collapsed check runs: the 4 alliance frames
// NavigateTo(alliance)/Capture/NavigateTo(alliance_members) spend, then
// NavigateTo(alliance_members)'s own two reads (WaitFor's arrival, and the
// confirming CurrentScreen its replan loop takes before returning — see
// threeGroupFrames' doc comment), then one more for the loop's own check.
func navToChevronCheckFrames(alliance, collapsed image.Image) []image.Image {
	return []image.Image{alliance, alliance, alliance, alliance, collapsed, collapsed, collapsed}
}

// A group that does not open fails with ErrGroupDidNotExpand rather than
// capturing whatever was already on screen.
func TestRosterCaptureFailsWhenAGroupDoesNotOpen(t *testing.T) {
	alliance := allianceScreenFrame()
	collapsed := rosterFrame(true, false) // chevron_expanded never appears

	frames := navToChevronCheckFrames(alliance, collapsed)
	frames = append(frames, rep(collapsed, 6)...) // 3 failed open attempts: tap verify + confirm, each

	rt, _, cap, rec := newRosterHarness(t, frames)
	fn, ok := Get("roster_capture")
	if !ok {
		t.Fatal("roster_capture not registered")
	}
	err := fn(context.Background(), rt)
	if !errors.Is(err, ErrGroupDidNotExpand) {
		t.Fatalf("got %v, want ErrGroupDidNotExpand", err)
	}
	// The alliance frame is captured up front, before the group loop —
	// that single capture is expected. What must not happen is a second
	// one, which would mean scrollCapture ran despite the group never
	// confirming open.
	if cap.nextID != 1 {
		t.Errorf("got %d frames captured, want 1 (only the alliance frame) — a group that did not open must not be captured", cap.nextID)
	}
	if len(rec.calls) != 0 {
		t.Errorf("got %d RecordCapture calls, want 0 — a failed group must not reach recordFrames", len(rec.calls))
	}
}

// The retry loop is bounded: a chevron that never flips fails the task
// after chevronTapAttempts rather than looping forever.
func TestRosterCaptureStopsRetryingAfterTheBoundRatherThanLoopingForever(t *testing.T) {
	alliance := allianceScreenFrame()
	collapsed := rosterFrame(true, false)

	frames := navToChevronCheckFrames(alliance, collapsed)
	frames = append(frames, rep(collapsed, 6)...)

	rt, tr, _, _ := newRosterHarness(t, frames)
	fn, ok := Get("roster_capture")
	if !ok {
		t.Fatal("roster_capture not registered")
	}
	err := fn(context.Background(), rt)
	if !errors.Is(err, ErrGroupDidNotExpand) {
		t.Fatalf("got %v, want ErrGroupDidNotExpand", err)
	}
	// Navigation's members_button tap plus exactly chevronTapAttempts
	// chevron taps proves the loop stopped at the bound rather than
	// hanging or retrying indefinitely.
	if got, want := countTaps(tr), 1+chevronTapAttempts; got != want {
		t.Errorf("got %d taps, want %d (1 navigation + %d bounded chevron attempts)", got, want, chevronTapAttempts)
	}
}
