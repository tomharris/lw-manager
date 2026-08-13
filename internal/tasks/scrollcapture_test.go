package tasks

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/capture"
	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// The screen scrollCapture's tests exercise. It is not one of
// runtimetest.Registry's screens — scrollCapture needs a tall, scrollable
// list region, which the shared 128x128 anchor grid was never built to
// simulate — so this file wires its own tiny registry and graph instead of
// using runtimetest.Options.
const scrollTestScreen = "vs_ranking_alliance"

// scrollFrameW/H are the synthetic frame's pixel size. Deliberately short:
// the anchor lives in the top 20% (the sticky header) and the striped list
// fills the region the tests scroll — {Y1: 0.2, Y2: 0.8} — leaving a usable
// height (region height minus one Pitch of 128) of just 76px. That narrow
// margin is what makes a genuine, non-degenerate overshoot reachable at all:
// vision.ScrollOffset can only ever report an offset up to a bit less than
// the region's own height, so proving `offset > usable` requires usable to
// sit well below that ceiling rather than to consume nearly all of it.
const (
	scrollFrameW = 120
	scrollFrameH = 340
	// scrollStripePeriod is the vertical period of the synthetic list's
	// pattern. A shift within one period is measured directly; a shift of
	// exactly one period is indistinguishable from no movement at all, which
	// is why period and pitch never share a factor the tests rely on.
	scrollStripePeriod = 419
)

// frameScript generates one synthetic scroll frame. shift is how far the
// list's content moves from the previous script entry, in pixels — 0 means
// the swipe was swallowed (or the list has already hit bottom).
type frameScript struct {
	shift int
}

// scrollFrame renders a synthetic vs_ranking_alliance frame: a fixed header
// anchor (unaffected by scrolling, so recognition survives every position)
// over a striped list region whose content is offset by cum pixels.
//
// The stripe pattern wraps with period scrollStripePeriod rather than
// growing without bound, so a long-running script (TestScrollCaptureStopsAtMaxFrames
// swipes dozens of times) never has to reason about an ever-larger pixel
// value — only the delta between two consecutive cum values is ever
// measured, and that delta is what each test controls via shift.
func scrollFrame(cum int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, scrollFrameW, scrollFrameH))
	for y := 0; y < scrollFrameH; y++ {
		yy := ((y+cum)%scrollStripePeriod + scrollStripePeriod) % scrollStripePeriod
		v := uint8((yy / 3 * 37) % 251)
		for x := 0; x < scrollFrameW; x++ {
			img.Set(x, y, color.RGBA{R: v, G: v, B: uint8((x / 5 * 11) % 251), A: 255})
		}
	}
	drawScrollAnchor(img)
	return img
}

// scrollAnchorRegion is the header's identifying anchor: fixed in place, well
// clear of the {Y1: 0.2, Y2: 0.8} list region every test scrolls, so it
// matches at every scroll position.
var scrollAnchorRegion = transport.Rect{X1: 0.1, Y1: 0.02, X2: 0.9, Y2: 0.15}

func drawScrollAnchor(img *image.RGBA) {
	// Match rounds a region's fractional bounds with math.Round; truncating
	// here instead can place row/column 0 of the drawn template one pixel
	// outside Match's own search rect, which knocks the score from a clean
	// 1.0 down into the 0.6-0.7 band — comfortably below the 0.9 threshold.
	x1 := int(math.Round(scrollAnchorRegion.X1 * scrollFrameW))
	y1 := int(math.Round(scrollAnchorRegion.Y1 * scrollFrameH))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			v := uint8((x*29 + y*61) % 256)
			img.Set(x1+x, y1+y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
}

func scrollAnchorTemplate() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			v := uint8((x*29 + y*61) % 256)
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// scrollRegistry is the minimal registry scrollCapture's tests need: one
// screen, one identifying anchor.
func scrollRegistry() *vision.Registry {
	return &vision.Registry{
		ReferenceHeight: scrollFrameH,
		Screens: []vision.Screen{{
			Name: scrollTestScreen,
			Anchors: []vision.Anchor{{
				ID:               "id",
				Template:         scrollAnchorTemplate(),
				Region:           scrollAnchorRegion,
				Threshold:        0.9,
				IdentifiesScreen: true,
			}},
		}},
	}
}

// scrollTransport adds a swipe count to *transport.ReplayTransport, which
// cannot itself gain a method from this package.
type scrollTransport struct {
	*transport.ReplayTransport
}

func (s *scrollTransport) SwipeCount() int {
	n := 0
	for _, a := range s.Actions() {
		if a.Kind == "swipe" {
			n++
		}
	}
	return n
}

// fakeCapturer hands out increasing screenshot ids without touching a
// database or a blob store, matching the fake runtime/capture_test.go uses.
type fakeCapturer struct {
	nextID int64
}

func (f *fakeCapturer) Record(ctx context.Context, accountID int64, img image.Image, screenID *string) (capture.Result, error) {
	f.nextID++
	return capture.Result{ScreenshotID: f.nextID, AccountID: accountID}, nil
}

// scrollTestRegion/scrollTestPitch mirror the ScrollSpec every test below
// builds, so expandScrollScript measures offsets exactly as scrollCapture
// will and can make the same complete/overshoot/cap decisions while staging
// frames.
var scrollTestRegion = transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8}

const scrollTestPitch = 128

// scrollTestUsable mirrors usableHeight for scrollTestRegion/scrollTestPitch
// at scrollFrameH.
var scrollTestUsable = int((scrollTestRegion.Y2-scrollTestRegion.Y1)*float64(scrollFrameH)) - scrollTestPitch

// expandScrollScript turns a script of logical swipe attempts into the exact
// sequence of raw frames scrollCapture will pull from the transport.
//
// One frameScript entry is not one Screenshot() call: captureFrame alone
// takes three (CurrentScreen's recognize, Capture's own re-verify, and the
// frame it hands back as the next prev — see the package doc comment on
// captureFrame), and the main loop takes a fourth to verify pre-swipe and a
// fifth to measure post-swipe, so a single moved frame costs five raw reads
// and a swallowed one costs two. Understaffing the transport at any of those
// points would desync every offset measured afterward, so this function
// walks scrollCapture's own control flow — measuring with the real
// vision.ScrollOffset — rather than assuming a fixed shape.
func expandScrollScript(script []frameScript) []image.Image {
	pos := 0
	cur := scrollFrame(pos)
	var imgs []image.Image
	pushCaptureFrame := func(img image.Image) {
		// CurrentScreen, Capture's verifyScreen, and the trailing Screenshot:
		// three reads of the same, unmoving frame.
		imgs = append(imgs, img, img, img)
	}
	pushCaptureFrame(cur) // the frame-0 capture before the loop

	zeroes := 0
	for seq := 1; seq < maxScrollFrames; seq++ {
		i := seq - 1
		if i >= len(script) {
			break // scrollCapture would keep going; the script just ends here
		}
		prev := cur
		pos += script[i].shift
		cur = scrollFrame(pos)
		imgs = append(imgs, prev) // swipeOnce's pre-swipe verify
		imgs = append(imgs, cur)  // the post-swipe measurement frame

		offset, err := vision.ScrollOffset(prev, cur, scrollTestRegion)
		if err != nil {
			panic("scrollcapture_test: script produced an unmeasurable frame pair: " + err.Error())
		}

		switch {
		case offset == 0:
			zeroes++
			if zeroes >= zeroOffsetRetries {
				return imgs // scrollCapture returns as soon as the bottom is proven
			}
			continue
		case offset > scrollTestUsable:
			return imgs // scrollCapture returns ErrScrollOvershot without capturing
		}
		zeroes = 0
		pushCaptureFrame(cur)
	}
	return imgs
}

// newScrollHarness stages the exact frame sequence script implies behind a
// ReplayTransport, and wires a Ctx against a registry that knows only
// scrollTestScreen. Modelled on ctxFor in helpers_test.go, but scrollCapture
// needs a screen and a scroll region ctxFor's shared registry does not have.
func newScrollHarness(t *testing.T, script []frameScript) (*runtime.Ctx, *scrollTransport) {
	t.Helper()
	frames := expandScrollScript(script)
	tr, err := transport.NewReplayTransportFromImages(frames...)
	if err != nil {
		t.Fatal(err)
	}

	c, err := runtime.New(runtime.Options{
		Transport:      tr,
		Registry:       scrollRegistry(),
		Graph:          &runtime.Graph{Entry: scrollTestScreen},
		Kill:           &runtimetest.FakeKill{},
		Capture:        &fakeCapturer{},
		AccountID:      1,
		Rand:           rand.New(rand.NewSource(7)),
		PollInterval:   time.Millisecond,
		WaitTimeout:    50 * time.Millisecond,
		RestartTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, &scrollTransport{tr}
}

// The list bottom must be proven, not assumed. A swallowed swipe and a real
// bottom both produce a zero offset, so the loop retries before believing it,
// exactly as startExecution retries a tap the game ignored.
func TestScrollCaptureRetriesBeforeBelievingTheBottom(t *testing.T) {
	rt, tr := newScrollHarness(t, []frameScript{
		{shift: 40},                        // moved
		{shift: 0},                         // swallowed swipe
		{shift: 40},                        // moved after the retry
		{shift: 0}, {shift: 0}, {shift: 0}, // three zeroes: the real bottom
	})

	frames, complete, err := scrollCapture(context.Background(), rt, ScrollSpec{
		Screen: "vs_ranking_alliance",
		Region: transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8},
		Pitch:  128,
	})
	if err != nil {
		t.Fatalf("scrollCapture: %v", err)
	}
	if !complete {
		t.Error("want complete = true — the bottom was reached")
	}
	if len(frames) < 3 {
		t.Errorf("got %d frames, want at least 3", len(frames))
	}
	if tr.SwipeCount() < 5 {
		t.Errorf("swipes = %d — the swallowed swipe must have been retried", tr.SwipeCount())
	}
}

func TestScrollCaptureFlagsAnOvershoot(t *testing.T) {
	// An offset larger than the usable region means rows were never on screen.
	// Recon proved this fires on the obvious gesture: 700px over 300ms moved
	// ~1504px against a ~990px viewport.
	rt, _ := newScrollHarness(t, []frameScript{{shift: 5000}})

	_, complete, err := scrollCapture(context.Background(), rt, ScrollSpec{
		Screen: "vs_ranking_alliance",
		Region: transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8},
		Pitch:  128,
	})
	if !errors.Is(err, ErrScrollOvershot) {
		t.Fatalf("got %v, want ErrScrollOvershot", err)
	}
	if complete {
		t.Error("an overshot capture is never complete")
	}
}

func TestScrollCaptureStopsAtMaxFrames(t *testing.T) {
	script := make([]frameScript, 0, maxScrollFrames+10)
	for i := 0; i < maxScrollFrames+10; i++ {
		script = append(script, frameScript{shift: 40})
	}
	rt, _ := newScrollHarness(t, script)

	frames, complete, err := scrollCapture(context.Background(), rt, ScrollSpec{
		Screen: "vs_ranking_alliance",
		Region: transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8},
		Pitch:  128,
	})
	if err != nil {
		t.Fatalf("scrollCapture: %v", err)
	}
	if complete {
		t.Error("hitting the frame cap is not a proven bottom")
	}
	if len(frames) > maxScrollFrames {
		t.Errorf("got %d frames, want at most %d", len(frames), maxScrollFrames)
	}
}
