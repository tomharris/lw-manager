package ingest

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"testing"

	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/transport"
)

// The colour-mask group-count reader's device-free tests.
//
// The segmentation half is checked against a REAL header -- capture 1's R2
// sticky band, the one whole-field OCR reads as "VN" -- because that is the
// only thing a synthetic bitmap could not honestly stand in for: the property
// being asserted is that this font's black outlines fall outside a
// bright-and-desaturated mask and leave the digit fills separated, which is a
// fact about the game's rendering and not about the code. The acceptance half
// is checked with ocr.FakeEngine, so the rules that decide whether a number
// becomes a creation budget are pinned without a tesseract binary present
// (CLAUDE.md invariant 6).

// loadRosterR2HeaderFrame returns a 720x1600 frame carrying capture 1's real
// R2 sticky group header ("R2) I'm Alright", count "1/11", collapse chevron)
// at exactly the rows groupHeaderRegion selects, on a background that is not
// the game's.
//
// Pasting a band into a full frame rather than testing the band alone is not
// ceremony: countDigitRuns takes its Y bounds as fractions of the frame
// height, so a 42px-tall image would have the band select two of its own rows
// and the test would measure nothing. The synthetic background is deliberately
// mid-grey -- neither bright enough to pass the mask nor saturated enough to
// pass the slash guard -- so anything the test observes came from the real
// header's pixels.
func loadRosterR2HeaderFrame(t *testing.T) image.Image {
	t.Helper()

	f, err := os.Open("testdata/roster_group_header_r2.png")
	if err != nil {
		t.Fatalf("opening the R2 group-header fixture: %v", err)
	}
	defer f.Close()
	band, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decoding the R2 group-header fixture: %v", err)
	}
	if got := band.Bounds().Dx(); got != 720 {
		t.Fatalf("fixture is %dpx wide, want the capture's 720; every X fraction in this"+
			" package is a fraction of the frame width and means nothing against another one", got)
	}
	wantH := int(groupHeaderRegion.Y2*1600) - int(groupHeaderRegion.Y1*1600)
	if got := band.Bounds().Dy(); got != wantH {
		t.Fatalf("fixture is %dpx tall, want the %dpx groupHeaderRegion selects on a 1600px frame", got, wantH)
	}

	frame := image.NewRGBA(image.Rect(0, 0, 720, 1600))
	draw.Draw(frame, frame.Bounds(), &image.Uniform{color.RGBA{R: 128, G: 128, B: 128, A: 255}}, image.Point{}, draw.Src)
	at := image.Rect(0, int(groupHeaderRegion.Y1*1600), 720, int(groupHeaderRegion.Y1*1600)+wantH)
	draw.Draw(frame, at, band, band.Bounds().Min, draw.Src)
	return frame
}

// TestCountDigitRunsSeparatesTheSlashFromEachDigit is the assertion the whole
// approach rests on. Capture 1's R2 header states "1/11"; whole-field OCR
// returns "VN", "Vu" or "VW" on all 21 frames of it because the outlined
// glyphs share their black outlines and tesseract's classifier merges them.
// The mask keeps only the white total, and each digit's own outline -- being
// neither bright nor desaturated -- falls outside it, so the fills come apart
// into separate column runs with no connected-component analysis.
//
// If this ever fails, nothing downstream can be salvaged by tuning: the
// premise that the outlines separate the fills has stopped holding, and
// `make probe-roster PROBE_ARGS=-roster.countsweep` is where to look next.
func TestCountDigitRunsSeparatesTheSlashFromEachDigit(t *testing.T) {
	runs := countDigitRuns(loadRosterR2HeaderFrame(t), groupHeaderRegion)

	// A slash and two digits. Not "at least three": a fourth run would mean
	// the mask caught something outside the count, which is exactly the shape
	// readGroupCountTotal refuses on.
	if len(runs) != 3 {
		t.Fatalf("countDigitRuns found %d ink runs on a real \"1/11\" header, want 3 (slash, digit, digit): %+v", len(runs), runs)
	}
	minW := int(countDigitMinWidthFrac * 720)
	maxW := int(countDigitMaxWidthFrac * 720)
	for i, r := range runs {
		if r.Width() < minW || r.Width() > maxW {
			t.Errorf("ink run %d is [%d,%d), %dpx wide, outside the %d..%dpx bound readGroupCountTotal enforces",
				i, r.X0, r.X1, r.Width(), minW, maxW)
		}
		if i > 0 && r.X0 <= runs[i-1].X1 {
			t.Errorf("ink run %d starts at x=%d, at or before run %d ends at x=%d: the runs are not separated,"+
				" so the digits merged and any total read from them is a guess", i, r.X0, i-1, runs[i-1].X1)
		}
	}
	// Right-aligned against the chevron, which is the guard that rejected the
	// three off-header bands -roster.countshift found producing a total.
	stripX1 := int(groupHeaderRegion.X2 * 720)
	if gap := stripX1 - runs[len(runs)-1].X1; gap > int(countRightGapMaxFrac*720) {
		t.Errorf("the last ink run ends %dpx short of the strip's right edge x=%d, further than countRightGapMaxFrac (%dpx) admits",
			gap, stripX1, int(countRightGapMaxFrac*720))
	}
}

// TestCountDigitRunsFindsNothingWithoutAHeader guards the direction that
// matters least on a passing run and most on a broken one: a mask that finds
// ink in flat background would hand readGroupCountTotal runs to read, and the
// engine would return something for them.
func TestCountDigitRunsFindsNothingWithoutAHeader(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 720, 1600))
	draw.Draw(frame, frame.Bounds(), &image.Uniform{color.RGBA{R: 128, G: 128, B: 128, A: 255}}, image.Point{}, draw.Src)
	if runs := countDigitRuns(frame, groupHeaderRegion); len(runs) != 0 {
		t.Errorf("countDigitRuns found %d ink runs in flat mid-grey, want none: %+v", len(runs), runs)
	}
}

// TestReadGroupCountTotalAssemblesDigitsLeftToRight pins the ORDER as well as
// the arithmetic. A reader that assembled right-to-left would be correct on
// "11" and silently wrong on every other two-digit total in the game, so the
// scripted digits differ from each other.
func TestReadGroupCountTotalAssemblesDigitsLeftToRight(t *testing.T) {
	// Three scripted reads, not two: two isolated digits and the whole-strip
	// read that has to corroborate them. See readGroupCountTotal on why the
	// second path exists.
	ing := New(nil, nil, &ocr.FakeEngine{Results: []ocr.Result{{Text: "6"}, {Text: "4"}, {Text: "/64"}}})
	got, err := ing.readGroupCountTotal(context.Background(), loadRosterR2HeaderFrame(t), groupHeaderRegion)
	if err != nil {
		t.Fatalf("readGroupCountTotal: %v", err)
	}
	if got != 64 {
		t.Errorf("readGroupCountTotal assembled %d from digits 6 then 4, want 64", got)
	}
}

// TestReadGroupCountTotalRefusesARunTheEngineDoesNotCallADigit is the measured
// failure, pinned as a REFUSAL rather than as a wrong number. Capture 1's R4
// header states "2/9" and PSM 8 classified the isolated "9" as "q" in the
// standalone prototype this reader came from. A count that gates member
// creation can survive not being read -- the group's rows go to the review
// queue -- and cannot survive being read wrongly, because a fabricated total
// silently withholds the rest of the group and nothing downstream checks it
// (CLAUDE.md's charset section, where "1/1" against a real group of 11 is the
// worked case).
func TestReadGroupCountTotalRefusesARunTheEngineDoesNotCallADigit(t *testing.T) {
	for _, read := range []string{"q", "", "11", "1x", " "} {
		ing := New(nil, nil, &ocr.FakeEngine{Results: []ocr.Result{{Text: "1"}, {Text: read}, {Text: "/11"}}})
		_, err := ing.readGroupCountTotal(context.Background(), loadRosterR2HeaderFrame(t), groupHeaderRegion)
		if !errors.Is(err, ErrUnparseable) {
			t.Errorf("a digit run read as %q gave err %v, want ErrUnparseable: anything but a refusal here"+
				" is a fabricated creation budget", read, err)
		}
	}
}

// TestReadGroupCountTotalRefusesWithoutColourProvingTheSlash is the guard whose
// absence would be invisible on this capture and catastrophic on another.
//
// The reader drops its first ink run because that run is the "/" between a
// SATURATED online count and a WHITE total. If a header ever drew its online
// count in white, the first run would be a digit and dropping it would divide
// the total by ten -- coherently, with no shape to refuse on. So the colour
// that justifies the drop has to be observed. This desaturates the header
// while leaving its geometry untouched, which is precisely the frame that
// would defeat a reader that assumed instead of checking.
func TestReadGroupCountTotalRefusesWithoutColourProvingTheSlash(t *testing.T) {
	src := loadRosterR2HeaderFrame(t)
	b := src.Bounds()
	grey := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			// Luma-preserving, so every mask decision is unchanged and only
			// the saturation the slash guard reads is gone.
			g := color.GrayModel.Convert(src.At(x, y)).(color.Gray)
			grey.Set(x, y, color.RGBA{R: g.Y, G: g.Y, B: g.Y, A: 255})
		}
	}
	if runs := countDigitRuns(grey, groupHeaderRegion); len(runs) != 3 {
		t.Fatalf("desaturating changed the segmentation (%d runs, want 3); this test would then be"+
			" measuring the mask rather than the slash guard", len(runs))
	}
	ing := New(nil, nil, &ocr.FakeEngine{Results: []ocr.Result{{Text: "1"}, {Text: "1"}, {Text: "/11"}}})
	_, err := ing.readGroupCountTotal(context.Background(), grey, groupHeaderRegion)
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("readGroupCountTotal accepted a header with no saturated online count, err = %v;"+
			" it must refuse, because its first run can no longer be shown to be the slash", err)
	}
}

// TestReadGroupCountTotalRefusesABandWithNoCount covers the whole-frame
// version of the segmentation guard: a band with nothing in it must not reach
// the engine at all.
func TestReadGroupCountTotalRefusesABandWithNoCount(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 720, 1600))
	draw.Draw(frame, frame.Bounds(), &image.Uniform{color.RGBA{R: 128, G: 128, B: 128, A: 255}}, image.Point{}, draw.Src)
	engine := &ocr.FakeEngine{Results: []ocr.Result{{Text: "9"}}}
	_, err := New(nil, nil, engine).readGroupCountTotal(context.Background(), frame, groupHeaderRegion)
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("readGroupCountTotal on a blank band gave err = %v, want ErrUnparseable", err)
	}
	// The engine must not have been consulted: a reader that segments nothing
	// and asks anyway is one bad scripted result away from inventing a count.
	if remaining := len(engine.Results); remaining != 1 {
		t.Errorf("the scripted engine was consulted on a band with no ink runs")
	}
}

// TestCountDigitSpecHasNoCharset joins the three assertions in charset_test.go
// for the same reason they exist: a whitelist is the obvious reach on a field
// of digits, and on this field it would remove the exact evidence the reader
// depends on. "q" for a real "9" is the engine declining to recognise a glyph;
// forced to digits it would name one instead, and the refusal that keeps a
// fabricated creation budget out is gone.
func TestCountDigitSpecHasNoCharset(t *testing.T) {
	if countDigitSpec.Charset != "" {
		t.Errorf("countDigitSpec.Charset = %q, want empty -- see its doc comment in groupcount.go."+
			" A whitelist here does not merely fail; it converts this reader's one safety property"+
			" (refusing an unrecognised glyph) into a wrong total that gates member creation.",
			countDigitSpec.Charset)
	}
}

// TestGroupCountBandIsDerivedFromTheHeaderRegion pins the coupling rather than
// the numbers. rankBadgeRegion's doc comment records a version of exactly this
// that hard-coded groupHeaderRegion's bounds and stayed put when they moved;
// the count strip shares the same right edge for the same physical reason (the
// gutter left of the collapse chevron), so it must move with it.
func TestGroupCountBandIsDerivedFromTheHeaderRegion(t *testing.T) {
	if countXFrac0 >= groupHeaderRegion.X2 {
		t.Fatalf("countXFrac0 %.4f is not left of groupHeaderRegion.X2 %.4f", countXFrac0, groupHeaderRegion.X2)
	}
	band := transport.Rect{X1: groupHeaderRegion.X1, Y1: groupHeaderRegion.Y1, X2: groupHeaderRegion.X2, Y2: groupHeaderRegion.Y2}
	if !band.Valid() {
		t.Fatalf("the header band %+v is not a valid unit-square rect", band)
	}
}

// TestReadGroupCountTotalRefusesWhenTheTwoReadPathsDisagree is the guard the
// threshold sweep forced into existence, and it is the difference between a
// reader that refuses and one that fabricates.
//
// `make probe-roster PROBE_ARGS=-roster.countsweep` moves the mask's luma
// threshold two levels either side of the shipped 240. The segmentation does
// not move -- capture 1's R2 count produces byte-identical ink runs at 238,
// 240 and 242 -- but the per-digit classification of one glyph goes "q", "1",
// "4", so a per-digit-only reader returns 14 for a real 11 at 242. With this
// rule the whole sweep reports zero wrong totals at every threshold and
// degrades to refusals instead.
func TestReadGroupCountTotalRefusesWhenTheTwoReadPathsDisagree(t *testing.T) {
	// Per-digit says 11, the whole strip says 14: exactly the 242 case.
	ing := New(nil, nil, &ocr.FakeEngine{Results: []ocr.Result{{Text: "1"}, {Text: "1"}, {Text: "/14"}}})
	_, err := ing.readGroupCountTotal(context.Background(), loadRosterR2HeaderFrame(t), groupHeaderRegion)
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("readGroupCountTotal accepted a per-digit read of 11 against a whole-strip read of 14, err = %v", err)
	}
}

// TestReadGroupCountTotalRetriesAWordReadThatFoundNoDigits pins the retry's
// trigger, which is NOT readFieldWithRetry's empty string. The count strip
// includes the slash's ink, so a corroborating read that fails on the digits
// still returns a character for it -- capture 1's R4 refused on exactly that,
// with a retry available and never firing, because res.Text was "|" rather
// than "".
func TestReadGroupCountTotalRetriesAWordReadThatFoundNoDigits(t *testing.T) {
	ing := New(nil, nil, &ocr.FakeEngine{Results: []ocr.Result{
		{Text: "1"}, {Text: "1"},
		{Text: "|"},   // whole strip, PSM 7: no digits, so the retry must run
		{Text: "/11"}, // whole strip, PSM 13
	}})
	got, err := ing.readGroupCountTotal(context.Background(), loadRosterR2HeaderFrame(t), groupHeaderRegion)
	if err != nil {
		t.Fatalf("readGroupCountTotal: %v", err)
	}
	if got != 11 {
		t.Errorf("readGroupCountTotal = %d, want 11 from the retried whole-strip read", got)
	}
}
