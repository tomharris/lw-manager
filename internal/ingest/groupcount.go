package ingest

import (
	"context"
	"fmt"
	"image"
	"strconv"
	"strings"

	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// The group header's "N/M" count, read from a colour mask one digit at a
// time, for the headers whole-field OCR cannot read at all.
//
// This is a RETRY behind parseGroupHeader, not a replacement for it. The
// reasoning for that, and the measurement behind the mask itself, are in
// vision.WhiteInkMask's doc comment; what lives here is the segmentation and
// the acceptance rules, because those are the part that decides whether a
// number is trustworthy enough to become a creation budget.
//
// WHY THE ACCEPTANCE RULES ARE STRUCTURAL AND NOT A THRESHOLD. total feeds
// groupTracker.expected, which gates member creation, and CLAUDE.md's charset
// section records what a fabricated count costs on this exact field: a
// "0123456789/" whitelist turned four unreadable R2 headers into "2 1/1" and
// "82 1/1", each parsing to a total of 1 against a real group of 11, which
// silently stops the other ten members being created and which no downstream
// check can catch. Failing to read is recoverable through the review queue;
// fabricating is not. So every rule below refuses on a shape it does not
// recognise rather than making the best of it.

// countMaskMinLuma and countMaskMaxSat are the mask thresholds, and THE TWO
// AXES DO NOT BEHAVE ALIKE. `make probe-roster PROBE_ARGS=-roster.countsweep`
// measures both across every real header in the capture:
//
//	maxSat   inert from 10 to 200, every luma row identical. The total is flat
//	         white (saturation 0) and the online count is cyan or green
//	         (saturation 140+), so 40 sits in a gap 100 wide. This one really
//	         is a midpoint of two separated populations.
//	minLuma  a SINGLE-POINT optimum. 240 reads all 61 header bands; 238 and
//	         242 each read 39 and refuse 22, 250 refuses all 61.
//
// The second line is worth stating plainly because the obvious reading of a
// well-chosen constant is that it sits on a plateau, and this one does not.
// What moves is not the segmentation -- the ink runs are byte-identical at
// 238, 240 and 242 -- but the THICKNESS of the surviving glyph, which is
// enough to change how the engine classifies it. An earlier version of this
// reader took the per-digit classification at face value and returned 14 for a
// real 11 at 242; readGroupCountTotal's two-path agreement rule is what turned
// that into a refusal, and with it in place the whole sweep reports zero wrong
// totals at every threshold. So the fragility is real and is contained by the
// acceptance rule rather than by this number being well chosen.
const (
	countMaskMinLuma = 240
	countMaskMaxSat  = 40
)

// countOnlineMinSat is how saturated a pixel must be to count as part of the
// ONLINE half of "N/M". It gates the guard that proves the first ink run is
// the slash -- see readGroupCountTotal.
const countOnlineMinSat = 60

// countXFrac0 is where the count strip begins, as a fraction of frame width.
// It is deliberately generous on the left: the mask, not this edge, is what
// excludes the group name, because the name is drawn in flat grey (luma ~120
// on both header styles) and never survives countMaskMinLuma. What this edge
// has to clear is the longest count the screen can state, and group sizes are
// user-controlled with no cap this package may invent (parseGroupHeader's own
// doc comment), so it is placed to admit a three-digit N -- "100/100" starts
// at x=520 of a 720px frame, and 0.70 is x=504.
//
// The right edge is groupHeaderRegion.X2 and is derived rather than restated,
// for the reason rankBadgeRegion derives its own Y bounds: a literal copy of
// that number has already drifted once in this package.
const countXFrac0 = 0.70

// countDigitMinWidthFrac and countDigitMaxWidthFrac bound one ink run's
// width, as fractions of frame width. Measured over capture 1's four headers,
// the slash runs are 8-9px and the digit runs 7-13px of a 720px frame; the
// bounds here are 5..20px.
//
// WHAT THIS DOES AND DOES NOT CATCH, because the tempting claim is wrong. It
// rejects a run far outside a glyph's size -- the off-header bands
// -roster.countshift produces include a 139px run of page background, and a
// 4px speckle. It does NOT reliably catch two MERGED digits: the narrow "1" of
// this font is 7px, so a merged "11" is about 17px and sits comfortably inside
// this bound. Widening the min or narrowing the max cannot fix that without
// also refusing the real 13px "6". Merging is caught by the two-path
// agreement rule in readGroupCountTotal instead, which is where a claim about
// it belongs.
const (
	countDigitMinWidthFrac = 5.0 / 720
	countDigitMaxWidthFrac = 20.0 / 720
)

// countDigitPadFrac is the white margin added either side of an isolated
// digit before it is handed to the engine, as a fraction of frame width.
// Tesseract reads a glyph flush against a crop edge markedly worse than one
// with room around it, and the mask makes the margin free: everything outside
// the glyph fill is already white.
const countDigitPadFrac = 4.0 / 720

// countRightGapMaxFrac is how far the last digit's right edge may sit from the
// right edge of the count strip, as a fraction of frame width. The count is
// right-aligned against the collapse chevron, and measured on capture 1's four
// real headers the gap is 9-10px of a 720px frame every time (last run ends at
// x=632 or 633, strip edge at x=642). 16px leaves headroom for a glyph that
// renders a pixel wider without admitting text that merely happens to be
// inside the strip.
//
// This guard was added on evidence, not on tidiness. `make probe-roster
// PROBE_ARGS=-roster.countshift` moves the band down onto the member rows,
// where there is no count to read, and three of those 183 bands produced a
// total anyway: two ink runs of 8px at x=568..588, read as "7", with the row's
// saturated avatar sitting to their left and satisfying the slash guard. They
// are 44px short of where a count's last digit ends, and nothing else about
// them is anomalous -- so alignment is the property that separates them, and
// it is a fact about the layout rather than a threshold fitted to those three.
const countRightGapMaxFrac = 16.0 / 720

// countMaxDigits bounds how many digit runs a total may have. Three admits
// every group size an alliance can hold (the alliance cap itself is 100) and
// refuses a strip that segmented into more runs than a count can have, which
// is the signal that the region caught something other than the count.
const countMaxDigits = 3

// countDigitSpec reads ONE already-isolated digit.
//
// No Charset, and that is deliberate rather than an oversight. A whitelist is
// the obvious reach on a field of digits and this package rejects it on three
// fields already, each on measurement (internal/ingest/charset_test.go). The
// rule CLAUDE.md draws from those is that a whitelist is safe only where every
// character it removes would also have been absent from a correct read -- and
// here the characters a whitelist would remove are precisely the evidence this
// reader depends on. "q" for a real "9" is the engine saying it does not
// recognise the glyph; a digit whitelist would have to put that ink somewhere
// and would return a digit instead. The refusal IS the safety property, so
// nothing may be done to make refusing harder.
//
// MinConf is 0 because the acceptance rule here is structural, not
// confidence-based: a single character that is a digit, in a run of the right
// width, in a strip proven to start with a slash. Tesseract's confidence on a
// one-glyph crop carries little and gating on it would only add a second,
// weaker reason to refuse.
var countDigitSpec = ocr.Spec{PSM: ocr.PSMSingleWord}

// countDigitOptions prepares an already-binary mask: upscale only.
//
// Every other step in the chain would be actively wrong on this input.
// Equalize stretches a two-level image into the same two levels, threshold
// re-decides a decision the mask has already made from colour (and would make
// it from luma, which is the axis that cannot see this field -- the whole
// point of the mask), and invert would hand tesseract white glyphs on black.
// Upscale is the one step that helps, for the reason it helps everywhere:
// these glyphs are ~10x22px at capture resolution.
var countDigitOptions = vision.Options{SkipEqualize: true, SkipThreshold: true, SkipInvert: true, UpscaleFactor: 8}

// countRun is one run of consecutive columns carrying mask ink, in absolute
// frame pixel coordinates: [X0, X1).
type countRun struct{ X0, X1 int }

// Width is the run's width in pixels.
func (r countRun) Width() int { return r.X1 - r.X0 }

// countDigitRuns returns the mask's ink runs across the count strip, in
// left-to-right order.
//
// A column carries ink if any pixel of it inside the band passes the mask. The
// runs are separated by ink-free columns, which for this font are the digits'
// own black outlines -- they fall outside a bright-and-desaturated mask, so
// the fills they enclose come apart without any connected-component analysis.
// That is the property vision.WhiteInkMask exists to produce, and this
// function is the only thing that consumes it.
//
// Split out from readGroupCountTotal so a test can assert the segmentation
// against a real header with no tesseract binary present: everything this
// package's unit tests can check about the count read, short of the digits
// themselves, is checkable here.
func countDigitRuns(img image.Image, band transport.Rect) []countRun {
	return countDigitRunsAt(img, band, countMaskMinLuma, countMaskMaxSat)
}

// countDigitRunsAt is countDigitRuns with the mask thresholds supplied, so
// `make probe-roster PROBE_ARGS=-roster.countsweep` can vary the two numbers
// the shipped constants fix. Production never calls it with anything but
// those constants.
func countDigitRunsAt(img image.Image, band transport.Rect, minLuma, maxSat int) []countRun {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	x0 := b.Min.X + int(countXFrac0*float64(w))
	x1 := b.Min.X + int(band.X2*float64(w))
	y0 := b.Min.Y + int(band.Y1*float64(h))
	y1 := b.Min.Y + int(band.Y2*float64(h))
	if x1 <= x0 || y1 <= y0 {
		return nil
	}
	mask, ok := vision.WhiteInkMask(img, minLuma, maxSat).(vision.Inker)
	if !ok {
		return nil
	}

	var runs []countRun
	inRun := false
	start := 0
	for x := x0; x <= x1; x++ {
		ink := false
		if x < x1 {
			for y := y0; y < y1; y++ {
				if mask.Ink(x, y) {
					ink = true
					break
				}
			}
		}
		switch {
		case ink && !inRun:
			inRun, start = true, x
		case !ink && inRun:
			inRun = false
			runs = append(runs, countRun{X0: start, X1: x})
		}
	}
	return runs
}

// readGroupCountTotal reads the M of a rank-group header's "N/M" by masking
// the count strip on colour and reading each surviving digit alone.
//
// band is the header's own rect: X bounds as groupHeaderRegion states them,
// Y bounds of the band this header actually occupies, which for an in-list
// header card is wherever its rank badge was found and not the sticky band.
//
// It refuses -- ErrUnparseable, wrapped with what it saw -- on every shape it
// does not recognise. The refusals in order, each guarding a different way a
// wrong number could be produced:
//
//  1. Fewer than two ink runs. A count needs a slash and at least one digit;
//     one run alone cannot be told apart from a stray.
//  2. No saturated pixel left of the first run. This is the load-bearing one.
//     Dropping the first run is only correct because that run is the "/"
//     between a saturated N and a white M -- if N were rendered white, the
//     first run would be a DIGIT and dropping it would divide the total by
//     ten, silently and coherently. So the colour that justifies the drop has
//     to be observed, not assumed. Every real header measured draws N in cyan
//     or green; this refuses rather than trusting that to hold forever.
//  3. The last run not ending flush against the strip's right edge, where a
//     right-aligned count has to end (countRightGapMaxFrac).
//  4. Too many digit runs (countMaxDigits), or a run outside the width band.
//  5. A digit run the engine does not return exactly one digit for. "q" for a
//     real "9" lands here, which is the measured behaviour and the intended
//     one.
func (i *Ingester) readGroupCountTotal(ctx context.Context, img image.Image, band transport.Rect) (int, error) {
	return i.readGroupCountTotalAt(ctx, img, band, countMaskMinLuma, countMaskMaxSat)
}

// readGroupCountTotalAt is readGroupCountTotal with the mask thresholds
// supplied, for -roster.countsweep. See countDigitRunsAt.
func (i *Ingester) readGroupCountTotalAt(ctx context.Context, img image.Image, band transport.Rect, minLuma, maxSat int) (int, error) {
	runs := countDigitRunsAt(img, band, minLuma, maxSat)
	if len(runs) < 2 {
		return 0, fmt.Errorf("ingest: group count mask: %d ink runs, want a slash and at least one digit: %w", len(runs), ErrUnparseable)
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	y0 := b.Min.Y + int(band.Y1*float64(h))
	y1 := b.Min.Y + int(band.Y2*float64(h))
	stripX0 := b.Min.X + int(countXFrac0*float64(w))
	if !hasSaturatedPixel(img, stripX0, runs[0].X0, y0, y1) {
		return 0, fmt.Errorf("ingest: group count mask: nothing saturated left of the first ink run, so it cannot be shown to be the slash: %w", ErrUnparseable)
	}

	digits := runs[1:]
	stripX1 := b.Min.X + int(band.X2*float64(w))
	if gap := stripX1 - runs[len(runs)-1].X1; gap > int(countRightGapMaxFrac*float64(w)) {
		return 0, fmt.Errorf("ingest: group count mask: last ink run ends %dpx short of the strip's right edge, further than a right-aligned count sits: %w", gap, ErrUnparseable)
	}
	if len(digits) > countMaxDigits {
		return 0, fmt.Errorf("ingest: group count mask: %d digit runs, want at most %d: %w", len(digits), countMaxDigits, ErrUnparseable)
	}
	minW := int(countDigitMinWidthFrac * float64(w))
	maxW := int(countDigitMaxWidthFrac * float64(w))
	pad := int(countDigitPadFrac * float64(w))
	mask := vision.WhiteInkMask(img, minLuma, maxSat)

	var sb strings.Builder
	for _, run := range append([]countRun{runs[0]}, digits...) {
		if run.Width() < minW || run.Width() > maxW {
			return 0, fmt.Errorf("ingest: group count mask: ink run [%d,%d) is %dpx, outside the %d..%dpx a glyph of this font occupies: %w",
				run.X0, run.X1, run.Width(), minW, maxW, ErrUnparseable)
		}
	}
	for _, run := range digits {
		rect := transport.Rect{
			X1: float64(run.X0-pad-b.Min.X) / float64(w),
			Y1: band.Y1,
			X2: float64(run.X1+pad-b.Min.X) / float64(w),
			Y2: band.Y2,
		}
		res, err := i.readField(ctx, mask, rect, countDigitSpec, countDigitOptions)
		if err != nil {
			return 0, err
		}
		d := strings.TrimSpace(res.Text)
		if len(d) != 1 || d[0] < '0' || d[0] > '9' {
			return 0, fmt.Errorf("ingest: group count mask: ink run [%d,%d) read %q, want exactly one digit: %w", run.X0, run.X1, res.Text, ErrUnparseable)
		}
		sb.WriteString(d)
	}

	// THE SECOND READ, and the reason it is not redundant.
	//
	// Per-digit classification alone is not stable. `make probe-roster
	// PROBE_ARGS=-roster.countsweep` moves the mask's luma threshold by two
	// levels either side of the shipped 240 and the SEGMENTATION does not
	// move at all -- capture 1's R2 count segments to exactly the same three
	// runs at 238, 240 and 242 -- while the classification of one isolated
	// glyph goes "q", "1", "4". At 242 the reader would have returned 14 for a
	// real 11: a coherent, in-range, silently wrong creation budget, which is
	// the one outcome this field cannot have.
	//
	// So the digits are read a second time as a single word across the whole
	// strip, and the two must agree. That is not two samples of one
	// measurement: PSM 8 on a one-glyph crop and PSM 7 on a several-glyph line
	// take different paths through tesseract, the second running the layout
	// analysis the first bypasses. They already disagree where the per-digit
	// path is weakest -- capture 1's R4 "9" reads "q" per-digit and "9" as a
	// word -- which is exactly the property being bought: a threshold that
	// pushes one path onto a wrong glyph does not push the other onto the same
	// wrong glyph, so a disagreement is the signature of an unreliable read
	// even when neither path reports low confidence.
	//
	// Agreement is required on the DIGITS, not on the raw text. The word read
	// carries the slash as a leading "/", "{" or a curly quote depending on
	// the threshold, and none of those is evidence about the number. Stripping
	// them here is not the charset-whitelist failure CLAUDE.md documents: a
	// whitelist changes what the ENGINE is allowed to see and forces ink into
	// permitted classes, while this discards characters the engine already
	// chose to emit, and the digit sequence that survives still has to match
	// the other path's exactly.
	word, err := i.readCountWord(ctx, mask, band, digits, pad, b, w)
	if err != nil {
		return 0, err
	}
	if word != sb.String() {
		return 0, fmt.Errorf("ingest: group count mask: per-digit read %q and whole-strip read %q disagree: %w", sb.String(), word, ErrUnparseable)
	}

	total, err := strconv.Atoi(sb.String())
	if err != nil || total <= 0 {
		// Unreachable given the per-run digit check above, which admits only
		// "0".."9" -- but a group of size zero renders no header to read at
		// all (fixtures/m4rostergate/README.md), so a total of 0 is a read
		// this must refuse rather than pass on as a creation budget of none.
		return 0, fmt.Errorf("ingest: group count mask: digits %q are not a positive total: %w", sb.String(), ErrUnparseable)
	}
	return total, nil
}

// readCountWord reads the whole digit strip -- every digit run, the slash
// excluded -- as one line, and returns just the digits of what came back.
//
// It is the corroborating half of readGroupCountTotal's two-path rule; that
// function's comment carries the measurement that makes it necessary.
func (i *Ingester) readCountWord(ctx context.Context, mask image.Image, band transport.Rect, digits []countRun, pad int, b image.Rectangle, w int) (string, error) {
	rect := transport.Rect{
		X1: float64(digits[0].X0-pad-b.Min.X) / float64(w),
		Y1: band.Y1,
		X2: float64(digits[len(digits)-1].X1+pad-b.Min.X) / float64(w),
		Y2: band.Y2,
	}
	// Retried on NO DIGITS rather than on the empty string, which is why this
	// is written out instead of calling readFieldWithRetry. The strip carries
	// the slash's own ink at its left edge, so PSM 7 going blind on the digits
	// still returns something -- a "|", a curly quote -- and a retry keyed on
	// res.Text being empty never fires. Measured: R4's one-digit "9" refused
	// on exactly that, with the retry sitting right there and never running.
	//
	// The widening is narrow and deliberate. "No digits at all" is the same
	// symptom readFieldWithRetry's empty string stands for -- the read
	// produced nothing about the field -- and it is not "retry a poor read",
	// which is the thing that rule exists to forbid. A read that produced any
	// digit is kept exactly as it came back.
	res, err := i.readField(ctx, mask, rect, countWordSpec, countDigitOptions)
	if err != nil {
		return "", err
	}
	if digitsOf(res.Text) == "" {
		res, err = i.readField(ctx, mask, rect, countWordRetrySpec, countDigitOptions)
		if err != nil {
			return "", err
		}
	}
	return digitsOf(res.Text), nil
}

// digitsOf keeps only the ASCII digits of a read.
func digitsOf(text string) string {
	var sb strings.Builder
	for _, r := range text {
		if r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// countWordSpec reads the digit strip as a single text line. It shares
// countDigitSpec's reasons for carrying no charset and no confidence floor,
// and differs only in page-segmentation mode -- which is the entire point of
// it, since a second read at the same PSM would corroborate nothing.
var countWordSpec = ocr.Spec{PSM: ocr.PSMSingleLine}

// countWordRetrySpec is the corroborating read's own retry, and neither PSM
// can be dropped in favour of the other. Measured over capture 1's 61 header
// frames, corroborating at PSM 7 alone refuses R4's one-digit "9" -- PSM 7
// returns the empty string on a single-glyph crop, the layout blindness
// internal/ocr/testdata/psm7_layout_blind.png exists to pin -- and
// corroborating at PSM 13 alone refuses all 21 of R2's frames, because raw
// line mode reads its "11" as "1". Retry-on-empty gets both: 61 correct, 0
// wrong, 0 refused.
//
// This is readFieldWithRetry's own rule and not a widening of it. The retry
// fires only on the empty string, so a corroborating read that produced
// something is never second-guessed, and the AGREEMENT requirement is applied
// afterwards either way -- whichever mode supplied the digits still has to
// match the per-digit path exactly. It is emphatically not "take the better of
// two", which on a numeric field is the manufacture CLAUDE.md forbids.
var countWordRetrySpec = ocr.Spec{PSM: ocr.PSMRawLine}

// hasSaturatedPixel reports whether any pixel in the half-open box carries
// enough colour to be one of the game's tinted glyphs.
func hasSaturatedPixel(img image.Image, x0, x1, y0, y1 int) bool {
	for x := x0; x < x1; x++ {
		for y := y0; y < y1; y++ {
			if vision.Saturated(img, x, y, countOnlineMinSat) {
				return true
			}
		}
	}
	return false
}
