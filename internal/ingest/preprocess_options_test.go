package ingest

import (
	"context"
	"fmt"
	"image"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// This file is the regression task 21 exists to prevent. capture 1's first
// real ingest read every group header as noise because readField passed
// only Region to vision.Preprocess, so every field silently got the full
// (equalize+threshold+invert) chain regardless of what its own Spec or
// comment said it needed. ocr.FakeEngine ignores the pixels a test hands it
// (its own doc comment), so no amount of scripting OCR results here would
// have caught that — the bug was in which Options reached Preprocess, not
// in what came back from the engine. These tests intercept at that seam
// instead: visionPreprocess is swapped for a spy that records the Options
// each readField call actually received, and the assertions are on those
// Options values, never on OCR text.

// preprocessCall is one recorded visionPreprocess call: the Options it
// received, and the dynamic type of the IMAGE it received.
//
// ImageType exists for the same reason the Options recording does. Which
// PIXELS a call site hands to Preprocess is as invisible to ocr.FakeEngine as
// which Options are -- the fake ignores the image entirely -- so a call site
// that stopped passing a transformed view of the frame would leave every test
// in this package green. The measured case is processRow's last-active retry,
// which must read vision.GreenChannel(img) and not img: mutating that one
// argument changes nothing any OCR assertion can see, while costing five of
// capture 1's twenty-four Online rows.
//
// A type name rather than the image itself: vision.GreenChannel returns an
// unexported type, so a test outside this file cannot name it, and the whole
// point is to pin that SOME transformation was applied and which one.
type preprocessCall struct {
	Opts      vision.Options
	ImageType string
}

// spyPreprocess installs a visionPreprocess that records every call's
// Options (with Region zeroed, since Region is computed per row/frame and
// tested separately by the parse/geometry tests) and image type, and still
// delegates to the real vision.Preprocess so the pixels handed to
// ocr.FakeEngine remain a valid image. It returns the recorded slice (grown in
// place) and a restore func the caller must defer.
func spyPreprocess(t *testing.T) (*[]preprocessCall, func()) {
	t.Helper()
	var calls []preprocessCall
	real := visionPreprocess
	visionPreprocess = func(img image.Image, opts vision.Options) *image.Gray {
		recorded := opts
		recorded.Region = transport.Rect{}
		calls = append(calls, preprocessCall{Opts: recorded, ImageType: fmt.Sprintf("%T", img)})
		return real(img, opts)
	}
	return &calls, func() { visionPreprocess = real }
}

// TestReadFieldSetsRegionWithoutMutatingCallersOptions confirms readField's
// one piece of logic beyond delegation: it must stamp opts.Region from its
// rect argument without disturbing any other field the caller set, and must
// not mutate the caller's Options value through aliasing (Options is passed
// by value here on purpose — a shared package-level var like nameOptions
// must not pick up a stray Region from whichever call happened to run last).
func TestReadFieldSetsRegionWithoutMutatingCallersOptions(t *testing.T) {
	calls, restore := spyPreprocess(t)
	defer restore()

	h := newHarness(t)
	h.engine.Results = []ocr.Result{{Text: "x", Confidence: 0.9}}

	img := rosterFrame(0)
	rect := transport.Rect{X1: 0.1, Y1: 0.1, X2: 0.5, Y2: 0.5}
	opts := vision.Options{SkipEqualize: true, UpscaleFactor: 7}
	optsBefore := opts

	if _, err := h.readField(context.Background(), img, rect, ocr.Spec{}, opts); err != nil {
		t.Fatalf("readField: %v", err)
	}

	if opts != optsBefore {
		t.Fatalf("readField mutated the caller's Options value: got %+v, want unchanged %+v", opts, optsBefore)
	}
	if len(*calls) != 1 {
		t.Fatalf("visionPreprocess called %d times, want 1", len(*calls))
	}
	got := (*calls)[0].Opts
	got.Region = transport.Rect{} // already zeroed by the spy; explicit for clarity
	want := vision.Options{SkipEqualize: true, UpscaleFactor: 7}
	if got != want {
		t.Errorf("Preprocess received Options %+v (Region aside), want %+v", got, want)
	}
}

// TestIngestRosterPassesEachFieldsMeasuredOptions is the wiring test the
// task-21 brief asked for: run one roster row through the real IngestRoster
// path and assert that every readField call carried its own field's
// package-level Options var — groupHeaderOptions, then nameOptions,
// powerOptions, levelOptions, lastActiveOptions for the row — in the exact
// order processRow issues them. A call site accidentally reusing another
// field's Options (or Options{}, the bug this task fixes) fails here even
// though every OCR text in the fixture is scripted to succeed, because
// ocr.FakeEngine's scripted results carry no information about what
// Options produced the pixels it "read".
func TestIngestRosterPassesEachFieldsMeasuredOptions(t *testing.T) {
	calls, restore := spyPreprocess(t)
	defer restore()

	h := newRosterIngestHarness(t, rosterFixture{group: "R1", groupTotal: 1, existing: 1})

	if _, err := h.IngestRoster(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	// nameSatOptions sits between the name and the power read: every row now
	// takes a second NAME read off a saturation mask (see nameSatMinSats), and
	// its options are its own -- a mask is binary, so the threshold and invert
	// steps fitted for luma are wrong on it.
	want := []vision.Options{groupHeaderOptions, nameOptions}
	for range nameSatMinSats {
		want = append(want, nameSatOptions)
	}
	want = append(want, powerOptions, levelOptions, lastActiveOptions)
	if len(*calls) != len(want) {
		t.Fatalf("visionPreprocess called %d times, want %d (calls: %+v)", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		if (*calls)[i].Opts != w {
			t.Errorf("call %d: got Options %+v, want %+v", i, (*calls)[i].Opts, w)
		}
	}
	// And the colour read must be a different IMAGE, not the same one with
	// different Options -- the same property the green-channel test below
	// asserts for the status column, and for the same reason: nothing about
	// Options or the scripted OCR text can show which pixels were read.
	for i := range nameSatMinSats {
		if got := (*calls)[2+i].ImageType; got != "vision.saturatedInkMask" {
			t.Errorf("extra name read %d preprocessed %s, want vision.saturatedInkMask:"+
				" it must read a saturation mask, which is the axis luma cannot reach", i, got)
		}
	}
}

// The last-active retry must read a DIFFERENT IMAGE, not the same image with
// different Options: vision.GreenChannel(img), because "Online" is green with
// a dark outline and luma leaves that green close to the cream card behind it.
//
// Nothing else in this package can catch a call site that reverts to `img`.
// ocr.FakeEngine ignores pixels, so the retry's scripted result comes back
// identical either way, and the Options are deliberately the same on both
// reads (lastActiveOptions) so an Options-only assertion cannot see the
// difference. This is the seam rankBadgeMinGap's own tests use for the same
// reason: a constant or an argument that only the pixels depend on needs a
// test that watches the pixels being chosen.
func TestIngestRosterLastActiveRetryReadsTheGreenChannel(t *testing.T) {
	calls, restore := spyPreprocess(t)
	defer restore()

	h := newHarness(t)
	h.stubRankFor("R1")
	h.addFrame(rosterFrame(1), 0)
	h.engine.Results = []ocr.Result{
		{Text: groupHeaderText("R1", 5), Confidence: 0.9},
		{Text: "Zephyr", Confidence: 0.9},
	}
	for range nameSatMinSats {
		h.engine.Results = append(h.engine.Results, inertSatRead)
	}
	h.engine.Results = append(h.engine.Results,
		ocr.Result{Text: "Power: 200.0M", Confidence: 0.9},
		ocr.Result{Text: "Lv.30", Confidence: 0.9},
		ocr.Result{Text: "oo", Confidence: 0.9}, // what luma makes of the wordmark
		ocr.Result{Text: "Online", Confidence: 0.9},
	)

	if _, err := h.IngestRoster(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	// header, name, one name-on-a-saturation-mask per nameSatMinSats entry,
	// power, level, last-active, last-active retry.
	wantCalls := 6 + len(nameSatMinSats)
	if len(*calls) != wantCalls {
		t.Fatalf("visionPreprocess called %d times, want %d (calls: %+v)", len(*calls), wantCalls, *calls)
	}
	primary, retry := (*calls)[wantCalls-2], (*calls)[wantCalls-1]
	if primary.Opts != lastActiveOptions || retry.Opts != lastActiveOptions {
		t.Errorf("both status reads must carry lastActiveOptions; got %+v and %+v", primary.Opts, retry.Opts)
	}
	if retry.ImageType == primary.ImageType {
		t.Fatalf("the retry preprocessed the same image type as the primary (%s): it must read vision.GreenChannel(img), and nothing about the Options or the OCR text can show that", retry.ImageType)
	}
	if !strings.Contains(retry.ImageType, "greenChannel") {
		t.Errorf("retry read image type %s, want vision's green-channel view", retry.ImageType)
	}
}

// TestIngestRosterPassesAllianceMemberCountOptions covers the alliance-frame
// read separately: readAllianceMemberCount is on its own path (it runs
// before the group-header loop, see IngestRoster's doc comment), so it is
// not exercised by the row-focused test above.
func TestIngestRosterPassesAllianceMemberCountOptions(t *testing.T) {
	calls, restore := spyPreprocess(t)
	defer restore()

	h := newRosterIngestHarness(t, rosterFixture{
		group: "R1", groupTotal: 1, existing: 1,
		allianceMemberCountText: "Members: 97/100",
	})

	if _, err := h.IngestRoster(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}

	if len(*calls) == 0 {
		t.Fatal("visionPreprocess was never called")
	}
	// The alliance frame is read first, ahead of any group header (see
	// newRosterIngestHarness's fixture-building comment).
	if (*calls)[0].Opts != allianceMemberCountOptions {
		t.Errorf("alliance-count read: got Options %+v, want %+v", (*calls)[0].Opts, allianceMemberCountOptions)
	}
}

// TestIngestVSPassesEachFieldsMeasuredOptions mirrors the roster test above
// for IngestVS's two fields.
func TestIngestVSPassesEachFieldsMeasuredOptions(t *testing.T) {
	calls, restore := spyPreprocess(t)
	defer restore()

	h := newVSIngestHarness(t, vsFixture{captureComplete: true, rosterSize: 1, rankedRows: 1})

	if _, err := h.IngestVS(context.Background(), 1, testPeriodKey); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}

	// Both vsNameModes entries share vsNameOptions (see vsNameModes's own
	// comment), so the name field now preprocesses twice with the same
	// Options before the points field's single call.
	want := []vision.Options{vsNameOptions, vsNameOptions, vsPointsOptions}
	if len(*calls) != len(want) {
		t.Fatalf("visionPreprocess called %d times, want %d (calls: %+v)", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		if (*calls)[i].Opts != w {
			t.Errorf("call %d: got Options %+v, want %+v", i, (*calls)[i].Opts, w)
		}
	}
}

// TestEveryFieldOptionsSkipsThreshold pins the headline result of task 21's
// measurement: on this UI, adaptive threshold was never the best-scoring
// choice on any field TestPreprocMeasure was run against — on the group
// header it was the specific step that destroyed a real crop
// (preprocess.go's doc comment), and on every other field a threshold-free
// shape tied or won outright (see each Options var's own comment in
// roster.go/vs.go for its number). This test is not a re-measurement; it
// exists so that anyone changing one of these vars later trips over this
// comment and the per-field numbers behind it, rather than silently
// reintroducing threshold on a field where it was measured not to help.
func TestEveryFieldOptionsSkipsThreshold(t *testing.T) {
	for name, opts := range map[string]vision.Options{
		"groupHeaderOptions":         groupHeaderOptions,
		"nameOptions":                nameOptions,
		"powerOptions":               powerOptions,
		"levelOptions":               levelOptions,
		"lastActiveOptions":          lastActiveOptions,
		"allianceMemberCountOptions": allianceMemberCountOptions,
		"vsNameOptions":              vsNameOptions,
		"vsPointsOptions":            vsPointsOptions,
	} {
		if !opts.SkipThreshold {
			t.Errorf("%s: SkipThreshold = false, want true (no field measured threshold as the best choice; see its Options var's comment for the number)", name)
		}
	}
}
