# M4 Analytics Collection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Capture the alliance roster and the weekly VS ranking from a real handset, parse them into append-only facts with screenshot provenance, and route every uncertain read to a human review queue.

**Architecture:** Two device-side capture tasks navigate, scroll and store frames plus their *measured* scroll offsets; a separate `control ingest` pass reads those frames from the blob store and writes facts. The split buys replay of a parser fix over historical captures, keeps the OCR subprocess out of the window where the game is open, and makes every ingest test device-free.

**Tech Stack:** Go 1.26.4, `CGO_ENABLED=0`, pgx/v5, goose migrations, `tesseract` CLI as a subprocess, hand-rolled NCC in `internal/vision`, `golang.org/x/text/unicode/norm` (already an indirect dependency, promoted to direct).

**Spec:** `docs/superpowers/specs/2026-08-12-m4-analytics-collection-design.md`
**Recon:** `docs/superpowers/specs/2026-08-12-m4-recon-findings.md`

## Global Constraints

Every task's requirements implicitly include this section.

- **`CGO_ENABLED=0`, always.** Enforced by the Makefile and `make verify-nocgo`. No gocv, no gosseract, no onnxruntime.
- **No absolute pixel coordinates outside a `Transport` implementation.** Everything upstream speaks `transport.Norm` / `transport.Rect`, both components in `[0,1]`. `Norm.Pixels` is the only sanctioned denormalization point. *Exception, and it is narrow:* `internal/vision` and `internal/ingest` work on decoded images and may compute in pixels internally, but every value that crosses a package boundary as a **coordinate** is normalized.

  A **distance measured within an image is not a coordinate** and may cross in pixels: `ScrollOffset`'s return and `capture_frames.offset_px` are scroll deltas, measured from one frame pair and only ever consumed alongside those same frames. The invariant exists to stop code hardcoding *where to tap* on a screen whose resolution varies; it is not a ban on integer image arithmetic. Anything that addresses a screen location — a tap target, an anchor box, a search region — is normalized without exception.

  The known cost, accepted: a persisted `offset_px` is meaningful only against the frames it was measured from, so it stops being self-describing if a second device with a different resolution joins the fleet. Revisit at that point, not before.
- **`context.Context` is the first parameter of anything that does I/O.**
- **Wrap errors with `%w`** and enough context to locate the failure without a stack trace: which device, which account, which key.
- **All logs go through `log/slog` to stderr. CLI results go to stdout** so they stay pipeable.
- **Sentinel errors** (`ErrNotFound`, `ErrOutOfRange`, `ErrAccountDisabled`, and the new ones here) are compared with `errors.Is`/`errors.As`, never by string.
- **Every task is idempotent and interruptible.** Assume the process is killed at any step.
- **No task acts without a matched screen anchor first.** Blind taps are a bug.
- **Facts are append-only.** Corrections supersede via `superseded_by`; nothing is mutated in place.
- **Every OCR-derived number carries a confidence and a screenshot reference.** Low-confidence reads go to the review queue, never to a leaderboard.
- **All vision logic ships with fixture-based tests that run with no device attached.** `go test ./...` must pass with no emulator, no adb, no Docker.
- **Sleeps go through the jittered context helper** (`rt.Sleep`). Never bare `time.Sleep` in task code.
- **The kill switch is checked between every task step.**
- **Tests never read `LW_DATABASE_URL`.** Integration tests use `internal/dbtest`, which reads `LW_TEST_DATABASE_URL` and refuses any database not named `*_test`.

---

## File Structure

**New packages**

| path | responsibility |
|---|---|
| `internal/ingest/segment.go` | split a frame's list region into row bands |
| `internal/ingest/parse.go` | pure field parsers: power, level, last-active, points |
| `internal/ingest/rows.go` | geometric dedupe across frames via measured offsets |
| `internal/ingest/roster.go` | roster route: rows → members + facts, group-count gating |
| `internal/ingest/vs.go` | VS route: rows → facts, absence-means-zero |
| `internal/ingest/ingest.go` | orchestration, confidence gating, review-queue writes |
| `internal/ingest/store.go` | the database surface ingest needs, as an interface |
| `internal/roster/normalize.go` | NFKC, mark stripping, whitespace collapse, casefold |
| `internal/roster/match.go` | token-set ratio, candidate ranking |

**Extended**

| path | change |
|---|---|
| `internal/vision/scroll.go` | new: `ScrollOffset` |
| `internal/vision/screens.go` | add `ScreenAllianceDuel` |
| `internal/runtime/graph.go` | six new edges |
| `internal/tasks/scrollcapture.go` | new: shared scroll-and-capture helper |
| `internal/tasks/roster_capture.go` | new task |
| `internal/tasks/vs_capture.go` | new task |
| `internal/db/migrations/00005_analytics.sql` | new tables |
| `internal/db/analytics.go` | hand-written pgx queries for the new tables |
| `internal/studio/review.go` | `/review` handlers |
| `cmd/control/main.go` | `ingest` subcommand |
| `templates/manifest.yaml` | four anchors, one recropped |

---

## Task Ordering

Tasks 1–4 are pure and device-free and can be done in any order. Task 5 (schema) is independent of them. Task 6 needs the handset. Tasks 7–9 need 1 and 6. Tasks 10–14 need 2–5. Task 15 needs 14. Tasks 16–17 close.

---

### Task 1: `vision.ScrollOffset`

Measures how far list content moved between two frames. This is the primitive the whole capture design rests on: recon showed fling roughly doubles a swipe, so the offset must be measured, never assumed.

**Files:**
- Create: `internal/vision/scroll.go`
- Test: `internal/vision/scroll_test.go`

**Interfaces:**
- Consumes: `vision.Match`, `vision.Grayscale`, `variance` (all existing, same package)
- Produces:
  - `func ScrollOffset(prev, cur image.Image, region transport.Rect) (int, error)` — pixels the content moved **up** between prev and cur; `0` means it did not move
  - `var ErrOffsetUncertain = errors.New("vision: scroll offset could not be measured")`

- [ ] **Step 1: Write the failing test**

Create `internal/vision/scroll_test.go`:

```go
package vision

import (
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// stripedFrame draws horizontal bands of varying grey so a vertical shift is
// unambiguous. A flat image would correlate with itself at every offset, which
// is the degenerate case ScrollOffset must not be tested against.
func stripedFrame(w, h, shift int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		// +shift moves content up: the band that was at y+shift is now at y.
		v := uint8((((y + shift) / 7) * 37) % 251)
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: v, G: v, B: uint8((x / 5 * 11) % 251), A: 255})
		}
	}
	return img
}

func TestScrollOffsetMeasuresAKnownShift(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8}
	prev := stripedFrame(120, 800, 0)
	cur := stripedFrame(120, 800, 64)

	got, err := ScrollOffset(prev, cur, region)
	if err != nil {
		t.Fatalf("ScrollOffset: %v", err)
	}
	if got != 64 {
		t.Fatalf("offset = %d, want 64", got)
	}
}

func TestScrollOffsetReturnsZeroWhenNothingMoved(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8}
	frame := stripedFrame(120, 800, 0)

	got, err := ScrollOffset(frame, frame, region)
	if err != nil {
		t.Fatalf("ScrollOffset: %v", err)
	}
	if got != 0 {
		t.Fatalf("offset = %d, want 0", got)
	}
}

// A change outside the region must not register as scrolling. This is the
// announcement banner recon caught animating in the header: while it runs,
// whole-frame comparison reports progress forever.
func TestScrollOffsetIgnoresChangeOutsideTheRegion(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.5, X2: 1, Y2: 0.9}
	prev := stripedFrame(120, 800, 0)
	cur := stripedFrame(120, 800, 0)
	for y := 0; y < 200; y++ {
		for x := 0; x < 120; x++ {
			cur.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}

	got, err := ScrollOffset(prev, cur, region)
	if err != nil {
		t.Fatalf("ScrollOffset: %v", err)
	}
	if got != 0 {
		t.Fatalf("offset = %d, want 0 — change outside the region must not count", got)
	}
}

func TestScrollOffsetRejectsAFlatRegion(t *testing.T) {
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.8}
	flat := image.NewRGBA(image.Rect(0, 0, 120, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 120; x++ {
			flat.Set(x, y, color.RGBA{R: 20, G: 20, B: 20, A: 255})
		}
	}

	if _, err := ScrollOffset(flat, flat, region); !errors.Is(err, ErrOffsetUncertain) {
		t.Fatalf("got %v, want ErrOffsetUncertain", err)
	}
}

func TestScrollOffsetRejectsAnInvalidRegion(t *testing.T) {
	frame := stripedFrame(120, 800, 0)
	bad := transport.Rect{X1: 0.9, Y1: 0.2, X2: 0.1, Y2: 0.8}

	if _, err := ScrollOffset(frame, frame, bad); err == nil {
		t.Fatal("want an error for an inverted region, got nil")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/vision/ -run TestScrollOffset -v`
Expected: FAIL — `undefined: ScrollOffset`

- [ ] **Step 3: Write the implementation**

Create `internal/vision/scroll.go`:

```go
package vision

import (
	"errors"
	"fmt"
	"image"

	"github.com/tomharris/lw-manager/internal/transport"
)

// ErrOffsetUncertain reports that no vertical shift could be measured with
// confidence. It is deliberately distinct from "the list did not move": a
// caller that cannot tell those apart would treat a failed measurement as a
// list bottom and truncate the capture silently.
var ErrOffsetUncertain = errors.New("vision: scroll offset could not be measured")

const (
	// offsetStripFrac is the height of the probe strip as a fraction of the
	// region. Large enough to carry structure, small enough that a full
	// viewport of travel still leaves it inside the previous frame.
	offsetStripFrac = 0.12
	// offsetMinScore is the NCC below which a placement is not believed.
	offsetMinScore = 0.90
	// offsetMinVariance rejects a probe strip flat enough to correlate with
	// anything, which is the same trap that makes a near-flat anchor useless:
	// NCC divides out the template's variance, so a flat strip asks "is this
	// area smooth" and every gap between cards answers yes.
	offsetMinVariance = 50.0
	// offsetProbes is how many candidate strip positions are considered; the
	// highest-variance one wins.
	offsetProbes = 3
)

// ScrollOffset measures how far the content inside region moved up between
// prev and cur, in pixels of the frames' own resolution. Zero means the
// content did not move.
//
// It works by cutting a probe strip from cur near the top of the region and
// finding where that strip sits in prev. Content moving up by d puts a feature
// that was at y+d in prev at y in cur, so the strip is searched downward only.
//
// This is measurement rather than assumption on purpose. Recon on the handset
// found that fling roughly doubles a swipe — a 700px gesture moved ~1504px
// against a ~990px viewport — so a capture that trusts its gesture skips rows
// while every frame still looks valid.
func ScrollOffset(prev, cur image.Image, region transport.Rect) (int, error) {
	if !region.Valid() {
		return 0, fmt.Errorf("vision: scroll region %+v is not a valid unit-square rect", region)
	}
	if prev.Bounds() != cur.Bounds() {
		return 0, fmt.Errorf("vision: frames differ in size: %v vs %v", prev.Bounds(), cur.Bounds())
	}

	h := cur.Bounds().Dy()
	regionTop := int(region.Y1 * float64(h))
	regionBot := int(region.Y2 * float64(h))
	regionH := regionBot - regionTop
	stripH := int(offsetStripFrac * float64(regionH))
	if stripH < 8 || regionH < 4*stripH {
		return 0, fmt.Errorf("vision: region is too short to measure a scroll offset (%d px): %w", regionH, ErrOffsetUncertain)
	}

	strip, stripY, err := bestProbeStrip(cur, region, regionTop, regionH, stripH)
	if err != nil {
		return 0, err
	}

	// Search prev from the strip's own row downward to the region's bottom.
	// Anything above stripY would mean the list scrolled backwards, which this
	// capture loop never does.
	search := transport.Rect{
		X1: region.X1,
		Y1: float64(stripY) / float64(h),
		X2: region.X2,
		Y2: region.Y2,
	}
	res, err := Match(prev, strip, search, h)
	if err != nil {
		return 0, fmt.Errorf("vision: matching the probe strip: %w", err)
	}
	if res.Score < offsetMinScore {
		return 0, fmt.Errorf("vision: best placement scored %.3f, below %.2f: %w", res.Score, offsetMinScore, ErrOffsetUncertain)
	}

	foundY := int(res.Box.Y1 * float64(h))
	d := foundY - stripY
	if d < 0 {
		d = 0
	}
	return d, nil
}

// bestProbeStrip picks the highest-variance candidate strip from cur, and
// refuses to return one flat enough to match anywhere.
func bestProbeStrip(cur image.Image, region transport.Rect, regionTop, regionH, stripH int) (image.Image, int, error) {
	w := cur.Bounds().Dx()
	x0 := int(region.X1 * float64(w))
	x1 := int(region.X2 * float64(w))

	type candidate struct {
		img image.Image
		y   int
		v   float64
	}
	var best candidate
	for i := 0; i < offsetProbes; i++ {
		y := regionTop + (i+1)*stripH
		sub := image.NewRGBA(image.Rect(0, 0, x1-x0, stripH))
		for yy := 0; yy < stripH; yy++ {
			for xx := 0; xx < x1-x0; xx++ {
				sub.Set(xx, yy, cur.At(cur.Bounds().Min.X+x0+xx, cur.Bounds().Min.Y+y+yy))
			}
		}
		v := variance(sub)
		if v > best.v {
			best = candidate{img: sub, y: y, v: v}
		}
	}
	if best.img == nil || best.v < offsetMinVariance {
		return nil, 0, fmt.Errorf("vision: probe strip variance %.1f below %.1f: %w", best.v, offsetMinVariance, ErrOffsetUncertain)
	}
	return best.img, best.y, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/vision/ -run TestScrollOffset -v`
Expected: PASS, all five.

- [ ] **Step 5: Run the whole vision suite for regressions**

Run: `go test ./internal/vision/`
Expected: `ok`

- [ ] **Step 6: Commit**

```bash
git add internal/vision/scroll.go internal/vision/scroll_test.go
git commit -m "Measure the scroll offset instead of trusting the gesture

Fling roughly doubles a swipe on the handset: a 700px gesture moved ~1504px
against a ~990px viewport, so a capture that assumes its gesture skips about a
third of the rows while every frame still looks valid.

ScrollOffset cuts a probe strip from the new frame and finds it in the previous
one, reusing the existing NCC rather than adding a second matcher. It takes a
region because bottom detection must ignore everything outside the list -- an
announcement banner animating in the header would otherwise report progress
forever -- and it refuses a flat probe strip for the same reason a flat anchor
is useless: NCC divides out the template variance, so a smooth strip matches
every gap between cards.

ErrOffsetUncertain is distinct from a zero offset on purpose. A caller that
conflated them would read a failed measurement as a list bottom and truncate
the capture silently."
```

---

### Task 2: Row segmentation

**Files:**
- Create: `internal/ingest/segment.go`
- Test: `internal/ingest/segment_test.go`

**Interfaces:**
- Consumes: `transport.Rect`
- Produces:
  - `type RowBand struct { Y0, Y1 int }`
  - `func SegmentRows(img image.Image, region transport.Rect, pitch int) ([]RowBand, error)`
  - `var ErrPitchMismatch = errors.New("ingest: detected rows do not match the expected pitch")`

`pitch` is the expected row height in pixels — 112 on the roster, 128 on the ranking. It is a **sanity check**, not the mechanism, so a layout change fails loudly instead of silently misaligning every field rect.

- [ ] **Step 1: Write the failing test**

Create `internal/ingest/segment_test.go`:

```go
package ingest

import (
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// cardFrame draws `n` dark cards of height `card` separated by light gaps of
// height `gap`, starting at y=top. This is the shape of both list screens:
// distinct cards on a lighter page background.
func cardFrame(w, h, top, card, gap, n int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	light := color.RGBA{R: 250, G: 240, B: 240, A: 255}
	dark := color.RGBA{R: 60, G: 70, B: 110, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, light)
		}
	}
	y := top
	for i := 0; i < n; i++ {
		for yy := y; yy < y+card && yy < h; yy++ {
			for x := 0; x < w; x++ {
				img.Set(x, yy, dark)
			}
		}
		y += card + gap
	}
	return img
}

func TestSegmentRowsFindsEveryCard(t *testing.T) {
	// 6 cards of 100px with 12px gaps, starting at y=200. Pitch is 112.
	img := cardFrame(200, 1000, 200, 100, 12, 6)
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	bands, err := SegmentRows(img, region, 112)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) != 6 {
		t.Fatalf("got %d bands, want 6: %+v", len(bands), bands)
	}
	if bands[0].Y0 != 200 {
		t.Errorf("first band starts at %d, want 200", bands[0].Y0)
	}
	for i, b := range bands {
		if got := b.Y1 - b.Y0; got != 100 {
			t.Errorf("band %d height = %d, want 100", i, got)
		}
	}
}

func TestSegmentRowsClipsToTheRegion(t *testing.T) {
	// Cards run the full frame, but the region excludes the top 300px, which
	// is how the sticky group header and the pinned self row are dropped.
	img := cardFrame(200, 1000, 0, 100, 12, 9)
	region := transport.Rect{X1: 0, Y1: 0.3, X2: 1, Y2: 1.0}

	bands, err := SegmentRows(img, region, 112)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	for _, b := range bands {
		if b.Y0 < 300 {
			t.Fatalf("band %+v starts above the region floor of 300", b)
		}
	}
}

func TestSegmentRowsRejectsAPitchThatDoesNotMatch(t *testing.T) {
	img := cardFrame(200, 1000, 200, 100, 12, 6)
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	// The real pitch is 112. A layout change that moved it to 160 must be
	// loud, because silently misaligned rows produce plausible wrong numbers.
	if _, err := SegmentRows(img, region, 160); !errors.Is(err, ErrPitchMismatch) {
		t.Fatalf("got %v, want ErrPitchMismatch", err)
	}
}

func TestSegmentRowsOnAnEmptyRegionReturnsNoBands(t *testing.T) {
	img := cardFrame(200, 1000, 0, 100, 12, 0)
	region := transport.Rect{X1: 0, Y1: 0.2, X2: 1, Y2: 0.9}

	bands, err := SegmentRows(img, region, 112)
	if err != nil {
		t.Fatalf("SegmentRows: %v", err)
	}
	if len(bands) != 0 {
		t.Fatalf("got %d bands, want 0", len(bands))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ingest/ -run TestSegmentRows -v`
Expected: FAIL — package does not exist / `undefined: SegmentRows`

- [ ] **Step 3: Write the implementation**

Create `internal/ingest/segment.go`:

```go
// Package ingest turns stored capture frames into facts. It never touches a
// device: everything here reads decoded images and writes rows, which is what
// keeps its tests device-free and lets a parser fix be replayed over every
// capture ever taken.
package ingest

import (
	"errors"
	"fmt"
	"image"
	"sort"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrPitchMismatch reports that the detected rows do not have the expected
// height. It is a layout-change alarm: silently misaligned rows would keep
// producing plausible numbers from the wrong pixels.
var ErrPitchMismatch = errors.New("ingest: detected rows do not match the expected pitch")

// pitchTolerance is how far a detected band may sit from the expected pitch
// before segmentation is disbelieved, as a fraction of the pitch.
const pitchTolerance = 0.25

// RowBand is one detected row, in pixel coordinates of the frame it came from.
type RowBand struct {
	Y0, Y1 int
}

// Height returns the band's height in pixels.
func (b RowBand) Height() int { return b.Y1 - b.Y0 }

// SegmentRows splits the list region into row bands by projecting brightness
// across each scanline and cutting at the light gaps between cards.
//
// pitch is the expected row height and is checked rather than assumed: a
// detected band more than pitchTolerance away from it means the layout moved,
// which must fail loudly. Recon measured 112px on the roster and 128px on the
// ranking.
func SegmentRows(img image.Image, region transport.Rect, pitch int) ([]RowBand, error) {
	if !region.Valid() {
		return nil, fmt.Errorf("ingest: list region %+v is not a valid unit-square rect", region)
	}
	if pitch <= 0 {
		return nil, fmt.Errorf("ingest: pitch must be positive, got %d", pitch)
	}

	g := vision.Grayscale(img)
	b := g.Bounds()
	top := b.Min.Y + int(region.Y1*float64(b.Dy()))
	bot := b.Min.Y + int(region.Y2*float64(b.Dy()))
	x0 := b.Min.X + int(region.X1*float64(b.Dx()))
	x1 := b.Min.X + int(region.X2*float64(b.Dx()))
	if bot-top < pitch/2 {
		return nil, nil
	}

	// Row-mean brightness across the region.
	means := make([]float64, 0, bot-top)
	for y := top; y < bot; y++ {
		var sum float64
		for x := x0; x < x1; x++ {
			sum += float64(g.GrayAt(x, y).Y)
		}
		means = append(means, sum/float64(x1-x0))
	}

	// Split at the midpoint between the darkest and brightest scanline. Cards
	// are darker than the page background on both screens, so "below the
	// midpoint" is card and "above" is gap. A midpoint is used rather than a
	// fixed constant so the same code works on either screen's palette.
	lo, hi := means[0], means[0]
	for _, m := range means {
		if m < lo {
			lo = m
		}
		if m > hi {
			hi = m
		}
	}
	if hi-lo < 8 {
		// The region is essentially uniform: no cards to find.
		return nil, nil
	}
	mid := (lo + hi) / 2

	var bands []RowBand
	inCard := false
	start := 0
	for i, m := range means {
		switch {
		case m < mid && !inCard:
			inCard, start = true, top+i
		case m >= mid && inCard:
			inCard = false
			bands = append(bands, RowBand{Y0: start, Y1: top + i})
		}
	}
	if inCard {
		bands = append(bands, RowBand{Y0: start, Y1: bot})
	}

	if len(bands) == 0 {
		return nil, nil
	}

	// Measure the observed pitch before judging anything by the expected one.
	// The expected pitch is the value under suspicion here, so it cannot also
	// be the yardstick: deciding slivers by it means that when the expectation
	// is wrong every band looks like a partial row, the list empties, and a
	// layout change is reported as "no rows found" instead of as the mismatch
	// it is.
	heights := make([]int, len(bands))
	for i, band := range bands {
		heights[i] = band.Height()
	}
	sort.Ints(heights)
	observed := heights[len(heights)/2]

	if delta := float64(observed-pitch) / float64(pitch); delta > pitchTolerance || delta < -pitchTolerance {
		return nil, fmt.Errorf("ingest: rows measure %d px against an expected pitch of %d: %w", observed, pitch, ErrPitchMismatch)
	}

	// Now drop slivers, judged against the observed pitch. A partial row
	// clipped by the region edge is short relative to its neighbours, which is
	// what "sliver" actually means, and the region edge is exactly how the
	// sticky group header and the pinned self row get excluded.
	whole := bands[:0]
	for _, band := range bands {
		if float64(band.Height()) >= float64(observed)*(1-pitchTolerance) {
			whole = append(whole, band)
		}
	}
	return whole, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/ingest/ -run TestSegmentRows -v`
Expected: PASS, all four.

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/segment.go internal/ingest/segment_test.go
git commit -m "Segment a list frame into row bands

Projects brightness across each scanline and cuts at the light gaps between
cards, which is easy on both list screens because the cards are distinctly
darker than the page. The split point is the midpoint between the darkest and
brightest scanline rather than a constant, so one implementation serves both
palettes.

The expected pitch -- 112px on the roster, 128px on the ranking -- is checked
rather than assumed. A layout change that moved it would otherwise keep
producing plausible numbers read from the wrong pixels, which is worse than a
failure. Partial rows clipped by the region edge are dropped rather than handed
downstream, since the region is exactly how the sticky group header and the
pinned self row get excluded."
```

---

### Task 3: Field parsers

**Files:**
- Create: `internal/ingest/parse.go`
- Test: `internal/ingest/parse_test.go`

**Interfaces:**
- Produces:
  - `func ParsePower(s string) (int64, error)` — `"Power: 216.2M"` → `216200000`
  - `func ParseLevel(s string) (int, error)` — `"Lv.35"` → `35`
  - `func ParseLastActiveHours(s string) (float64, error)` — `"5h ago"` → `5`, `"Online"` → `0`
  - `func ParsePoints(s string) (int64, error)` — `"45,048,150"` → `45048150`
  - `var ErrUnparseable = errors.New("ingest: field could not be parsed")`

- [ ] **Step 1: Write the failing test**

Create `internal/ingest/parse_test.go`:

```go
package ingest

import (
	"errors"
	"testing"
)

func TestParsePower(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"Power: 216.2M", 216_200_000},
		{"216.2M", 216_200_000},
		{"Power: 1.5B", 1_500_000_000},
		{"Power: 987.6K", 987_600},
		{"Power: 232.2M", 232_200_000},
	}
	for _, c := range cases {
		got, err := ParsePower(c.in)
		if err != nil {
			t.Errorf("ParsePower(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePower(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParsePowerRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "Power:", "Power: M", "banana"} {
		if _, err := ParsePower(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePower(%q) = %v, want ErrUnparseable", in, err)
		}
	}
}

func TestParseLevel(t *testing.T) {
	// A bare "35" is deliberately NOT accepted. Without the Lv prefix there is
	// nothing distinguishing a level from any other number that bled into the
	// crop, and ParseLevel("Power: 216.2M Lv.35") returning 216 is exactly the
	// silent wrong answer the anchoring exists to prevent.
	for in, want := range map[string]int{"Lv.35": 35, "Lv.4": 4, "LV 35": 35, "lv35": 35} {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseLastActiveHours(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"Online", 0},
		{"online", 0},
		{"1h ago", 1},
		{"14h ago", 14},
		{"30m ago", 0.5},
		{"2d ago", 48},
	}
	for _, c := range cases {
		got, err := ParseLastActiveHours(c.in)
		if err != nil {
			t.Errorf("ParseLastActiveHours(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLastActiveHours(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParsePoints(t *testing.T) {
	for in, want := range map[string]int64{
		"45,048,150": 45_048_150,
		"16,831,113": 16_831_113,
		"0":          0,
		"1,524,375":  1_524_375,
	} {
		got, err := ParsePoints(in)
		if err != nil {
			t.Errorf("ParsePoints(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePoints(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParsePointsRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", ",", "abc"} {
		if _, err := ParsePoints(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePoints(%q) = %v, want ErrUnparseable", in, err)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ingest/ -run TestParse -v`
Expected: FAIL — `undefined: ParsePower`

- [ ] **Step 3: Write the implementation**

Create `internal/ingest/parse.go`:

```go
package ingest

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrUnparseable reports that a field's raw OCR text did not have the shape
// its parser expects. It is a sentinel so callers can route the row to review
// rather than guessing a value.
var ErrUnparseable = errors.New("ingest: field could not be parsed")

// Every pattern is anchored, and that is the whole point. An unanchored
// pattern extracts characters rather than validating shape, which cannot tell
// "the field I expected" from "some digits that happened to be nearby" — and
// the failure is silent, producing a confident wrong number.
//
// That defeats invariant #5 in a way confidence cannot catch. The invariant
// keeps bad numbers off leaderboards via OCR confidence, but a parse error
// happens *after* OCR: tesseract can report 0.98 on a crisp "2162M" it read
// perfectly, and an unanchored parser then returns 2,162,000,000 — ten times
// the truth, carrying that high confidence with it. A field that does not
// match its expected shape must return ErrUnparseable so the row goes to
// review, which is where an unreadable number belongs.
var (
	powerRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([KMB])$`)
	levelRe = regexp.MustCompile(`(?i)^LV\.?\s*([0-9]+)$`)
	pointsRe = regexp.MustCompile(`^(?:[0-9]{1,3}(?:,[0-9]{3})*|[0-9]+)$`)
	agoRe   = regexp.MustCompile(`^([0-9]+)\s*([hmd])`)
	powerLabelRe = regexp.MustCompile(`(?i)^\s*POWER\s*:?\s*`)
)

// ParsePower reads the abbreviated power the member list shows, e.g.
// "Power: 216.2M".
//
// The game never shows full precision here, so the result carries at most four
// significant figures: 216.2M is 216,200,000 give or take 50,000. That is a
// property of the screen, recorded rather than worked around, and it is below
// the weekly deltas any derived metric cares about.
func ParsePower(s string) (int64, error) {
	m := powerRe.FindStringSubmatch(strings.ToUpper(s))
	if m == nil {
		return 0, fmt.Errorf("ingest: power %q: %w", s, ErrUnparseable)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("ingest: power %q: %w", s, ErrUnparseable)
	}
	switch m[2] {
	case "K":
		v *= 1e3
	case "M":
		v *= 1e6
	case "B":
		v *= 1e9
	}
	return int64(v), nil
}

// ParseLevel reads "Lv.35".
func ParseLevel(s string) (int, error) {
	m := levelRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("ingest: level %q: %w", s, ErrUnparseable)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("ingest: level %q: %w", s, ErrUnparseable)
	}
	return n, nil
}

// ParseLastActiveHours reads the relative last-active label and returns hours
// ago. "Online" is zero.
//
// Hours-ago is stored rather than a derived timestamp so the fact stays equal
// to what the screenshot shows, which is what makes it checkable against that
// screenshot later. Resolution is about an hour.
func ParseLastActiveHours(s string) (float64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	if strings.HasPrefix(t, "online") {
		return 0, nil
	}
	m := agoRe.FindStringSubmatch(t)
	if m == nil {
		return 0, fmt.Errorf("ingest: last-active %q: %w", s, ErrUnparseable)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("ingest: last-active %q: %w", s, ErrUnparseable)
	}
	switch m[2] {
	case "m":
		return n / 60, nil
	case "h":
		return n, nil
	case "d":
		return n * 24, nil
	}
	return 0, fmt.Errorf("ingest: last-active %q: %w", s, ErrUnparseable)
}

// ParsePoints reads a full-precision VS score such as "45,048,150". Unlike
// power, the ranking shows every digit.
func ParsePoints(s string) (int64, error) {
	t := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	if t == "" {
		return 0, fmt.Errorf("ingest: points %q: %w", s, ErrUnparseable)
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ingest: points %q: %w", s, ErrUnparseable)
	}
	return n, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/ingest/ -run TestParse -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/parse.go internal/ingest/parse_test.go
git commit -m "Parse the four member-list and ranking fields

Power is abbreviated on screen -- 216.2M, never a full number -- so ParsePower
returns at most four significant figures and the +/-50,000 of quantization is a
property of the game recorded rather than worked around. Points on the ranking
are full precision, so they get a separate parser rather than sharing one.

Last-active is stored as hours-ago rather than as a derived timestamp, so the
fact stays equal to what the screenshot shows and can still be checked against
it later. Online is zero.

Every parser returns ErrUnparseable rather than a zero value, so a row with a
mangled field routes to review instead of contributing a confident wrong
number to a leaderboard."
```

---

### Task 4: Name normalization and fuzzy matching

**Files:**
- Create: `internal/roster/normalize.go`, `internal/roster/match.go`
- Test: `internal/roster/normalize_test.go`, `internal/roster/match_test.go`

**Interfaces:**
- Produces:
  - `func Normalize(s string) string`
  - `func TokenSetRatio(a, b string) int` — 0..100 on **already-normalized** input
  - `type Candidate struct { MemberID int64; Name string; Score int }`
  - `type Member struct { ID int64; Name string; Aliases []string }`
  - `func Rank(raw string, members []Member) []Candidate` — best score per member, descending
  - `const AutoAccept = 92`, `const ReviewFloor = 75`

- [ ] **Step 1: Write the failing tests**

Create `internal/roster/normalize_test.go`:

```go
package roster

import "testing"

func TestNormalizeCollapsesLetterSpacing(t *testing.T) {
	// Recon frame 03: this member's name renders letter-spaced on screen and
	// OCR reads the spaces. Collapsing them is the single highest-value
	// normalization step, because it turns an unmatchable string into an exact
	// match without any fuzzy scoring at all.
	if got, want := Normalize("M I C H E L L"), "michell"; got != want {
		t.Errorf("Normalize = %q, want %q", got, want)
	}
}

func TestNormalizeCasefolds(t *testing.T) {
	if Normalize("Kain445") != Normalize("KAIN445") {
		t.Error("normalization must be case-insensitive")
	}
}

func TestNormalizeStripsCombiningMarks(t *testing.T) {
	if got, want := Normalize("Zérö"), "zero"; got != want {
		t.Errorf("Normalize = %q, want %q", got, want)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	for _, s := range []string{"M I C H E L L", "Zérö", "  Kain445  ", "O Nyankopo N"} {
		once := Normalize(s)
		if twice := Normalize(once); once != twice {
			t.Errorf("Normalize(%q) not idempotent: %q then %q", s, once, twice)
		}
	}
}

func TestNormalizeKeepsDistinctNamesDistinct(t *testing.T) {
	if Normalize("Kalor13") == Normalize("Kain445") {
		t.Error("normalization must not collapse genuinely different names")
	}
}
```

Create `internal/roster/match_test.go`:

```go
package roster

import "testing"

func TestTokenSetRatioIsPerfectOnEqualStrings(t *testing.T) {
	if got := TokenSetRatio("kain445", "kain445"); got != 100 {
		t.Errorf("got %d, want 100", got)
	}
}

func TestTokenSetRatioIgnoresTokenOrder(t *testing.T) {
	if got := TokenSetRatio("beast the mq", "mq the beast"); got != 100 {
		t.Errorf("got %d, want 100 — token order must not matter", got)
	}
}

func TestTokenSetRatioToleratesOneBadCharacter(t *testing.T) {
	// The realistic OCR failure: one glyph misread. This must stay well above
	// the review floor or every capture floods the queue.
	if got := TokenSetRatio("kaln445", "kain445"); got < ReviewFloor {
		t.Errorf("got %d, want >= %d", got, ReviewFloor)
	}
}

func TestTokenSetRatioSeparatesDifferentNames(t *testing.T) {
	if got := TokenSetRatio("kalor13", "kain445"); got >= AutoAccept {
		t.Errorf("got %d, want < %d — distinct members must not auto-accept", got, AutoAccept)
	}
}

func TestRankPrefersAnAliasOverAWeakerNameMatch(t *testing.T) {
	members := []Member{
		{ID: 1, Name: "Zero Orca", Aliases: []string{"zerooroa"}},
		{ID: 2, Name: "Zebra"},
	}
	got := Rank("zerooroa", members)
	if len(got) == 0 || got[0].MemberID != 1 {
		t.Fatalf("want member 1 first, got %+v", got)
	}
	if got[0].Score != 100 {
		t.Errorf("alias should match exactly, got %d", got[0].Score)
	}
}

func TestRankIsSortedDescending(t *testing.T) {
	members := []Member{
		{ID: 1, Name: "Kalor13"},
		{ID: 2, Name: "Kain445"},
		{ID: 3, Name: "Kain446"},
	}
	got := Rank("kain445", members)
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Fatalf("not sorted descending: %+v", got)
		}
	}
	if got[0].MemberID != 2 {
		t.Errorf("want member 2 first, got %d", got[0].MemberID)
	}
}

func TestRankOnAnEmptyRosterReturnsNoCandidates(t *testing.T) {
	if got := Rank("anyone", nil); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/roster/ -v`
Expected: FAIL — package does not exist

- [ ] **Step 3: Write normalization**

Create `internal/roster/normalize.go`:

```go
// Package roster matches an OCR-read name to a known alliance member.
//
// Last War names are full of unicode decoration, letter spacing, homoglyphs
// and alliance tags, and OCR adds its own noise on top. Normalization does
// most of the work here; the fuzzy score only has to cover what survives it.
//
// The matcher is hand-rolled rather than vendored, consistent with the
// hand-rolled NCC in internal/vision and with CGO_ENABLED=0.
package roster

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Normalize reduces a displayed name to a comparable form: compatibility
// decomposition, combining marks removed, non-alphanumerics dropped, and all
// whitespace collapsed away, casefolded.
//
// Collapsing internal whitespace entirely is the highest-value step and is
// deliberate rather than incidental: the member list renders some names
// letter-spaced ("M I C H E L L"), and OCR faithfully reports the spaces.
// Removing them turns an unmatchable string into an exact match with no fuzzy
// scoring involved at all.
func Normalize(s string) string {
	d := norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(d))
	for _, r := range d {
		switch {
		case unicode.Is(unicode.Mn, r):
			// A combining mark: drop it, so "é" has already become "e" + mark
			// and we keep only the "e".
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			// Spaces, punctuation, emoji and decorations are all dropped.
		}
	}
	return b.String()
}

// NormalizeTokens is Normalize but preserving word boundaries, for the token
// set ratio. Decoration is still stripped; only runs of whitespace survive, as
// single spaces.
func NormalizeTokens(s string) string {
	d := norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(d))
	prevSpace := false
	for _, r := range d {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevSpace = false
		case unicode.IsSpace(r):
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
```

- [ ] **Step 4: Write matching**

Create `internal/roster/match.go`:

```go
package roster

import (
	"sort"
	"strings"
)

// Thresholds from design doc §5. A confirmation in the review queue writes an
// alias, so tomorrow's identical misread matches directly and accuracy
// compounds instead of being re-tuned.
const (
	// AutoAccept is the score at or above which a match is taken without a
	// human.
	AutoAccept = 92
	// ReviewFloor is the score at or above which a match is offered for human
	// confirmation. Below it the row is rejected outright.
	ReviewFloor = 75
)

// Member is the matcher's view of a known member: an ID, a display name, and
// any aliases a human has previously confirmed.
type Member struct {
	ID      int64
	Name    string
	Aliases []string
}

// Candidate is one scored match.
type Candidate struct {
	MemberID int64
	Name     string
	Score    int
}

// TokenSetRatio scores two names in 0..100, ignoring token order. Input is
// normalized internally, so callers may pass raw text.
func TokenSetRatio(a, b string) int {
	ta, tb := strings.Fields(NormalizeTokens(a)), strings.Fields(NormalizeTokens(b))
	sort.Strings(ta)
	sort.Strings(tb)
	sa, sb := strings.Join(ta, ""), strings.Join(tb, "")
	if sa == "" && sb == "" {
		return 100
	}
	if sa == "" || sb == "" {
		return 0
	}
	if sa == sb {
		return 100
	}
	d := levenshtein(sa, sb)
	longest := len(sa)
	if len(sb) > longest {
		longest = len(sb)
	}
	score := 100 * (longest - d) / longest
	if score < 0 {
		score = 0
	}
	return score
}

// Rank scores raw against every member, taking each member's best score across
// its display name and aliases, and returns them sorted best-first.
func Rank(raw string, members []Member) []Candidate {
	out := make([]Candidate, 0, len(members))
	for _, m := range members {
		best := TokenSetRatio(raw, m.Name)
		for _, a := range m.Aliases {
			if s := TokenSetRatio(raw, a); s > best {
				best = s
			}
		}
		out = append(out, Candidate{MemberID: m.ID, Name: m.Name, Score: best})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// levenshtein is the standard two-row edit distance.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/roster/ -v`
Expected: PASS

- [ ] **Step 6: Tidy the module so x/text becomes a direct dependency**

Run: `go mod tidy && git diff --stat go.mod`
Expected: `golang.org/x/text` moves out of the indirect block. No new modules downloaded.

- [ ] **Step 7: Verify no-CGO still holds**

Run: `make verify-nocgo`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/roster go.mod go.sum
git commit -m "Match an OCR-read name to a known member

Normalization does most of the work and the fuzzy score only covers what
survives it. Collapsing internal whitespace is the single highest-value step
rather than an incidental one: the member list renders some names letter-spaced
and OCR faithfully reports the spaces, so removing them turns an unmatchable
string into an exact match with no scoring involved. NFKD plus combining-mark
removal handles the decorated names the roster is full of.

Scoring is a token set ratio over Levenshtein, hand-rolled for the same reasons
the NCC is: no CGO, and the alternative is a dependency for a hundred lines.
Thresholds are the design doc's 92 auto-accept and 75 review floor. Rank takes
each member's best score across its display name and its confirmed aliases,
which is what makes human confirmations compound -- tomorrow's identical
misread matches directly instead of queueing again.

x/text was already an indirect dependency, so this promotes it rather than
adding one."
```

---

### Task 5: Schema

**Files:**
- Create: `internal/db/migrations/00005_analytics.sql`, `internal/db/analytics.go`
- Test: `internal/db/analytics_integration_test.go`

**Interfaces:**
- Produces (on `*db.Pool`):
  - `func (p *Pool) UpsertAlliance(ctx context.Context, a Alliance) (int64, error)`
  - `func (p *Pool) CreateCapture(ctx context.Context, c Capture) (int64, error)`
  - `func (p *Pool) AddCaptureFrame(ctx context.Context, f CaptureFrame) error`
  - `func (p *Pool) FinishCapture(ctx context.Context, id int64, status string, parsed int, errMsg string) error`
  - `func (p *Pool) ListMembers(ctx context.Context, allianceID int64) ([]Member, error)`
  - `func (p *Pool) CreateMember(ctx context.Context, m Member) (int64, error)`
  - `func (p *Pool) AddAlias(ctx context.Context, memberID int64, alias, source string) error`
  - `func (p *Pool) InsertFact(ctx context.Context, f Fact) (int64, error)`
  - `func (p *Pool) QueueReview(ctx context.Context, r ReviewItem) (int64, error)`
  - Types `Alliance`, `Capture`, `CaptureFrame`, `Member`, `Fact`, `ReviewItem`

- [ ] **Step 1: Write the migration**

Create `internal/db/migrations/00005_analytics.sql`:

```sql
-- +goose Up

-- The alliance we observe. member_count is the "96/100" read off the alliance
-- screen, which is the reconciliation ground truth for the roster route.
CREATE TABLE alliances (
  id           bigserial PRIMARY KEY,
  tag          text NOT NULL,
  name         text NOT NULL,
  server       text,
  member_count int,
  observed_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tag, name)
);

-- Members are a dimension, not a fact: they are mutable and soft-deleted.
-- Only the roster route writes this table. A VS row that matches nothing goes
-- to review rather than creating a member, because one OCR misread would
-- otherwise mint a phantom that then accumulates facts and corrupts the very
-- count meant to catch it.
CREATE TABLE members (
  id              bigserial PRIMARY KEY,
  alliance_id     bigint NOT NULL REFERENCES alliances(id),
  name            text NOT NULL,
  name_normalized text NOT NULL,
  rank            text,
  first_seen_at   timestamptz NOT NULL DEFAULT now(),
  left_at         timestamptz,
  active          boolean NOT NULL DEFAULT true
);
CREATE INDEX members_alliance_active_idx ON members (alliance_id, active);
CREATE INDEX members_normalized_idx ON members (name_normalized);

-- Every human confirmation writes one of these, which is the mechanism that
-- makes matching accuracy compound rather than needing to be re-tuned.
CREATE TABLE member_aliases (
  id               bigserial PRIMARY KEY,
  member_id        bigint NOT NULL REFERENCES members(id),
  alias            text NOT NULL,
  alias_normalized text NOT NULL,
  source           text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (member_id, alias_normalized)
);

CREATE TABLE captures (
  id            bigserial PRIMARY KEY,
  account_id    bigint NOT NULL REFERENCES accounts(id),
  route         text NOT NULL,
  started_at    timestamptz NOT NULL DEFAULT now(),
  ended_at      timestamptz,
  status        text NOT NULL DEFAULT 'running',
  expected_rows int,
  parsed_rows   int,
  error         text,
  CONSTRAINT captures_status_check
    CHECK (status IN ('running','complete','partial','failed'))
);
CREATE INDEX captures_route_started_idx ON captures (route, started_at DESC);

-- offset_px is stored rather than recomputed at ingest time on purpose: a
-- later change to vision.ScrollOffset would otherwise silently re-segment
-- historical captures into different rows and make old facts unreproducible.
-- group_key carries the rank group, because on the roster route the rank
-- belongs to the frame's sticky header rather than to any row.
CREATE TABLE capture_frames (
  id            bigserial PRIMARY KEY,
  capture_id    bigint NOT NULL REFERENCES captures(id) ON DELETE CASCADE,
  seq           int NOT NULL,
  screenshot_id bigint NOT NULL REFERENCES screenshots(id),
  offset_px     int NOT NULL DEFAULT 0,
  group_key     text,
  UNIQUE (capture_id, seq)
);

CREATE TABLE participation_facts (
  id            bigserial PRIMARY KEY,
  member_id     bigint NOT NULL REFERENCES members(id),
  metric        text NOT NULL,
  value         numeric NOT NULL,
  observed_at   timestamptz NOT NULL,
  period_key    text NOT NULL,
  source        text NOT NULL,
  screenshot_id bigint REFERENCES screenshots(id),
  confidence    real NOT NULL,
  superseded_by bigint REFERENCES participation_facts(id),
  UNIQUE (member_id, metric, period_key, source, observed_at)
);
CREATE INDEX facts_member_metric_idx ON participation_facts (member_id, metric, period_key);
CREATE INDEX facts_live_idx ON participation_facts (metric, period_key) WHERE superseded_by IS NULL;

CREATE TABLE review_queue (
  id              bigserial PRIMARY KEY,
  capture_id      bigint REFERENCES captures(id) ON DELETE CASCADE,
  screenshot_id   bigint NOT NULL REFERENCES screenshots(id),
  row_y0          int NOT NULL,
  row_y1          int NOT NULL,
  raw_text        text NOT NULL,
  candidates_json jsonb NOT NULL DEFAULT '[]'::jsonb,
  reason          text NOT NULL,
  status          text NOT NULL DEFAULT 'pending',
  resolved_by     text,
  resolved_at     timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT review_status_check
    CHECK (status IN ('pending','resolved','rejected'))
);
CREATE INDEX review_pending_idx ON review_queue (status, created_at) WHERE status = 'pending';

-- +goose Down
DROP TABLE review_queue;
DROP TABLE participation_facts;
DROP TABLE capture_frames;
DROP TABLE captures;
DROP TABLE member_aliases;
DROP TABLE members;
DROP TABLE alliances;
```

- [ ] **Step 2: Write the failing integration test**

Create `internal/db/analytics_integration_test.go`:

```go
//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/dbtest"
)

func TestFactsAreAppendOnlyAndSupersede(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Prepare(ctx, t, db.Migrate)

	allianceID, err := pool.UpsertAlliance(ctx, db.Alliance{Tag: "OrCa", Name: "Organized Chaos", MemberCount: 96})
	if err != nil {
		t.Fatalf("UpsertAlliance: %v", err)
	}
	memberID, err := pool.CreateMember(ctx, db.Member{AllianceID: allianceID, Name: "Kain445", NameNormalized: "kain445", Rank: "R3"})
	if err != nil {
		t.Fatalf("CreateMember: %v", err)
	}

	obs := time.Now().UTC().Truncate(time.Second)
	first, err := pool.InsertFact(ctx, db.Fact{
		MemberID: memberID, Metric: "vs_points", Value: 60158133,
		ObservedAt: obs, PeriodKey: "2026-W33", Source: "ocr:vs_ranking", Confidence: 0.94,
	})
	if err != nil {
		t.Fatalf("InsertFact: %v", err)
	}

	// A correction is a new row that supersedes, never an update in place.
	second, err := pool.InsertFact(ctx, db.Fact{
		MemberID: memberID, Metric: "vs_points", Value: 60158134,
		ObservedAt: obs.Add(time.Second), PeriodKey: "2026-W33", Source: "manual", Confidence: 1.0,
	})
	if err != nil {
		t.Fatalf("InsertFact (correction): %v", err)
	}
	if err := pool.SupersedeFact(ctx, first, second); err != nil {
		t.Fatalf("SupersedeFact: %v", err)
	}

	live, err := pool.LiveFacts(ctx, "vs_points", "2026-W33")
	if err != nil {
		t.Fatalf("LiveFacts: %v", err)
	}
	if len(live) != 1 || live[0].ID != second {
		t.Fatalf("live facts = %+v, want only the superseding row %d", live, second)
	}
}

func TestUpsertAllianceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Prepare(ctx, t, db.Migrate)

	a := db.Alliance{Tag: "OrCa", Name: "Organized Chaos", MemberCount: 96}
	first, err := pool.UpsertAlliance(ctx, a)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	a.MemberCount = 95
	second, err := pool.UpsertAlliance(ctx, a)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("ids differ: %d then %d — upsert must not create a duplicate", first, second)
	}
}

func TestCaptureFramesCascadeWithTheirCapture(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Prepare(ctx, t, db.Migrate)

	accountID := dbtest.SeedAccount(ctx, t, pool)
	captureID, err := pool.CreateCapture(ctx, db.Capture{AccountID: accountID, Route: "vs_ranking", ExpectedRows: 96})
	if err != nil {
		t.Fatalf("CreateCapture: %v", err)
	}
	shotID := dbtest.SeedScreenshot(ctx, t, pool, accountID)
	if err := pool.AddCaptureFrame(ctx, db.CaptureFrame{CaptureID: captureID, Seq: 0, ScreenshotID: shotID, OffsetPx: 0}); err != nil {
		t.Fatalf("AddCaptureFrame: %v", err)
	}
	if err := pool.FinishCapture(ctx, captureID, "complete", 94, ""); err != nil {
		t.Fatalf("FinishCapture: %v", err)
	}

	frames, err := pool.CaptureFrames(ctx, captureID)
	if err != nil {
		t.Fatalf("CaptureFrames: %v", err)
	}
	if len(frames) != 1 || frames[0].ScreenshotID != shotID {
		t.Fatalf("frames = %+v", frames)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `docker compose up -d && make test-integration`
Expected: FAIL — `undefined: db.Alliance` etc.

- [ ] **Step 4: Add the seed helpers to dbtest**

`internal/dbtest` needs `SeedAccount` and `SeedScreenshot` if they do not already exist. Check first:

Run: `grep -n "func Seed" internal/dbtest/*.go`

If absent, add to `internal/dbtest/seed.go`:

```go
package dbtest

import (
	"context"
	"testing"

	"github.com/tomharris/lw-manager/internal/db"
)

// SeedAccount inserts the device, app instance and account rows an analytics
// test needs, and returns the account id. Integration tests truncate freely,
// so this is called per test rather than once.
func SeedAccount(ctx context.Context, t *testing.T, pool *db.Pool) int64 {
	t.Helper()
	var deviceID, instanceID, accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO devices (serial, transport, resolution_w, resolution_h, status)
		 VALUES ('TESTSERIAL', 'adb', 720, 1600, 'online') RETURNING id`).Scan(&deviceID); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO app_instances (device_id, package) VALUES ($1, 'com.fun.lastwar.gp') RETURNING id`,
		deviceID).Scan(&instanceID); err != nil {
		t.Fatalf("seed app instance: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (app_instance_id, nickname, role, enabled)
		 VALUES ($1, 'testalt', 'alliance_data', true) RETURNING id`,
		instanceID).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return accountID
}

// SeedScreenshot inserts one screenshot row and returns its id.
func SeedScreenshot(ctx context.Context, t *testing.T, pool *db.Pool, accountID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO screenshots (account_id, captured_at, object_key, sha256)
		 VALUES ($1, now(), 'test/key.png', repeat('a', 64)) RETURNING id`,
		accountID).Scan(&id); err != nil {
		t.Fatalf("seed screenshot: %v", err)
	}
	return id
}
```

Adjust the column lists to match the actual `00001_init.sql` schema — read it first:

Run: `sed -n '1,70p' internal/db/migrations/00001_init.sql`

- [ ] **Step 5: Write the query layer**

Create `internal/db/analytics.go` with the types and methods named in the Interfaces block above. Follow the hand-written pgx style already in the package. Key bodies:

```go
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Alliance is the observed alliance. MemberCount is the "96/100" read off the
// alliance screen and is the roster route's reconciliation ground truth.
type Alliance struct {
	ID          int64
	Tag         string
	Name        string
	Server      string
	MemberCount int
}

type Member struct {
	ID             int64
	AllianceID     int64
	Name           string
	NameNormalized string
	Rank           string
	Active         bool
}

type Capture struct {
	ID           int64
	AccountID    int64
	Route        string
	Status       string
	ExpectedRows int
	ParsedRows   int
	Error        string
}

type CaptureFrame struct {
	ID           int64
	CaptureID    int64
	Seq          int
	ScreenshotID int64
	OffsetPx     int
	GroupKey     string
}

type Fact struct {
	ID           int64
	MemberID     int64
	Metric       string
	Value        float64
	ObservedAt   time.Time
	PeriodKey    string
	Source       string
	ScreenshotID int64
	Confidence   float64
}

type ReviewItem struct {
	ID           int64
	CaptureID    int64
	ScreenshotID int64
	RowY0, RowY1 int
	RawText      string
	Candidates   any
	Reason       string
}

func (p *Pool) UpsertAlliance(ctx context.Context, a Alliance) (int64, error) {
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO alliances (tag, name, server, member_count, observed_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (tag, name) DO UPDATE
		  SET member_count = EXCLUDED.member_count, observed_at = now()
		RETURNING id`,
		a.Tag, a.Name, nullString(a.Server), a.MemberCount).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: upserting alliance %s/%s: %w", a.Tag, a.Name, err)
	}
	return id, nil
}

func (p *Pool) InsertFact(ctx context.Context, f Fact) (int64, error) {
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO participation_facts
		  (member_id, metric, value, observed_at, period_key, source, screenshot_id, confidence)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		f.MemberID, f.Metric, f.Value, f.ObservedAt, f.PeriodKey, f.Source,
		nullInt64(f.ScreenshotID), f.Confidence).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: inserting %s fact for member %d in %s: %w", f.Metric, f.MemberID, f.PeriodKey, err)
	}
	return id, nil
}

// SupersedeFact points an old fact at its correction. Nothing is mutated in
// place beyond this pointer, so every number still traces to the screenshot it
// came from.
func (p *Pool) SupersedeFact(ctx context.Context, old, replacement int64) error {
	_, err := p.Exec(ctx,
		`UPDATE participation_facts SET superseded_by = $2 WHERE id = $1`, old, replacement)
	if err != nil {
		return fmt.Errorf("db: superseding fact %d with %d: %w", old, replacement, err)
	}
	return nil
}

func (p *Pool) LiveFacts(ctx context.Context, metric, periodKey string) ([]Fact, error) {
	rows, err := p.Query(ctx, `
		SELECT id, member_id, metric, value, observed_at, period_key, source,
		       coalesce(screenshot_id, 0), confidence
		FROM participation_facts
		WHERE metric = $1 AND period_key = $2 AND superseded_by IS NULL
		ORDER BY value DESC`, metric, periodKey)
	if err != nil {
		return nil, fmt.Errorf("db: listing live %s facts for %s: %w", metric, periodKey, err)
	}
	defer rows.Close()

	var out []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.ID, &f.MemberID, &f.Metric, &f.Value, &f.ObservedAt,
			&f.PeriodKey, &f.Source, &f.ScreenshotID, &f.Confidence); err != nil {
			return nil, fmt.Errorf("db: scanning fact: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (p *Pool) QueueReview(ctx context.Context, r ReviewItem) (int64, error) {
	blob, err := json.Marshal(r.Candidates)
	if err != nil {
		return 0, fmt.Errorf("db: encoding review candidates: %w", err)
	}
	var id int64
	err = p.QueryRow(ctx, `
		INSERT INTO review_queue
		  (capture_id, screenshot_id, row_y0, row_y1, raw_text, candidates_json, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		nullInt64(r.CaptureID), r.ScreenshotID, r.RowY0, r.RowY1, r.RawText, blob, r.Reason).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: queueing review for screenshot %d: %w", r.ScreenshotID, err)
	}
	return id, nil
}
```

Add the remaining methods (`CreateCapture`, `AddCaptureFrame`, `FinishCapture`, `CaptureFrames`, `ListMembers`, `CreateMember`, `AddAlias`, `PendingReviews`, `ResolveReview`) in the same shape. Add `nullString` / `nullInt64` helpers if the package does not already have equivalents.

- [ ] **Step 6: Run the integration test to verify it passes**

Run: `make test-integration`
Expected: PASS

- [ ] **Step 7: Verify unit tests still pass with nothing running**

Run: `docker compose down && make test`
Expected: `ok` throughout — nothing in `make test` may need Postgres.

- [ ] **Step 8: Commit**

```bash
git add internal/db internal/dbtest
git commit -m "Add the analytics schema

Members are a dimension and mutable; facts are append-only and supersede via
superseded_by, so every number still traces to the screenshot it came from.
A partial unique index on superseded_by IS NULL is what makes reading the live
set cheap.

capture_frames stores the measured offset rather than letting ingest recompute
it. Recomputation would let a later change to ScrollOffset silently re-segment
historical captures into different rows, which would make old facts
unreproducible -- the thing append-only storage exists to prevent. group_key
carries the rank group because on the roster route the rank belongs to the
frame's sticky header rather than to any row.

captures.status is constrained rather than free text: partial is load-bearing
downstream, since a partial VS capture must not have its absences read as
zeroes."
```

---

### Task 6: Screens, anchors and graph edges

Needs the handset. This is the only task that requires a device and a human at a browser.

**Files:**
- Modify: `internal/vision/screens.go`, `internal/runtime/graph.go`, `templates/manifest.yaml`
- Modify: `fixtures/corpus/index.yaml` (regenerated)

**Interfaces:**
- Produces: `vision.ScreenAllianceDuel = "alliance_duel"`; anchors `alliance/members_button`, `alliance_duel/ranking_button`, `vs_ranking/weekly_tab`; recropped `vs_ranking_weekly/vs_ranking_alliance_button`; six graph edges.

- [ ] **Step 1: Pull the corpus before recording anything**

Run: `./bin/agent corpus pull`
Expected: frames materialize under `fixtures/corpus/<label>/`.

**This is not optional.** `agent record` rewrites `index.yaml` from a scan of the working tree, so recording into a corpus that was never pulled silently drops every labelled entry from the committed index. It looks like a clean index rather than a truncation.

- [ ] **Step 2: Confirm the handset is awake and unlocked**

```bash
adb devices
adb shell dumpsys power | grep -o 'mStayOn=[a-z]*'
adb shell dumpsys window | grep -o 'isKeyguardShowing=[a-z]*' | head -1
```
Expected: one device, `mStayOn=true`, `isKeyguardShowing=false`.

If the keyguard is showing, clear it — `adb shell locksettings clear --old <pin>`. Recon found `stayon` does **not** reliably prevent this handset from locking, and a `FLAG_SECURE` PIN screen returns a zero-byte capture, so a locked device fails at PNG decode rather than at recognition.

- [ ] **Step 3: Record frames of the Alliance Duel screen**

```bash
./bin/agent record --interval 2s --duration 3m
```

While it runs, navigate to the Alliance Duel screen (`base` → VS button) and leave it there, moving between its tabs. Ten or more frames of a new screen is the working minimum.

- [ ] **Step 4: Label the new frames and crop the anchors**

```bash
./bin/agent studio --addr 0.0.0.0:8088
```

In the browser:
1. Label the new frames `alliance_duel`.
2. Crop `alliance_duel/alliance_duel` — the "ALLIANCE DUEL" wordmark, an identifying anchor.
3. Crop `alliance_duel/ranking_button` — the Ranking button in the bottom bar.
4. Crop `alliance/members_button` — the Members button, text plus icon.
5. Crop `vs_ranking/weekly_tab` — the "Weekly Rank" tab **as it appears on `vs_ranking`**, where it is unselected. The existing `weekly_tab` anchors live on the two screens below and are the *selected* state.
6. **Recrop** `vs_ranking_weekly/vs_ranking_alliance_button` to include the "Your Alliance" label text, not just the checkbox.

Step 6 is the important one. The current crop is an empty checkbox at stddev 2,346, about 11× flatter than a text anchor, reading `worst-in 1.000 / best-out 1.000`. As a scoring anchor that merely inflates a screen's score; as a **tap target** it is worse — NCC lands its best match on an arbitrary smooth region and the task taps there while invariant #3 believes an anchor matched.

7. **Recrop the VS tree's identifying anchors:** `vs_ranking/vs_ranking`, `vs_ranking_weekly/vs_ranking_weekly`, `vs_ranking_alliance/vs_ranking_alliance`, and `weekly_tab` on both screens that declare it.

Step 7 is not optional polish, and the reason is measured. `agent score` at 1.0.357 reports:

```
anchor                      screen               worst-in  best-out  action
daily_tab                   vs_ranking           0.998     0.889     ok
vs_ranking                  vs_ranking           1.000     1.000     RECROP
vs_ranking_alliance         vs_ranking_alliance  1.000     1.000     RECROP
vs_ranking_weekly           vs_ranking_weekly    1.000     1.000     RECROP
weekly_tab                  vs_ranking_alliance  1.000     1.000     RECROP
weekly_tab                  vs_ranking_weekly    1.000     1.000     RECROP
vs_ranking_alliance_button  vs_ranking_weekly    1.000     1.000     RECROP
vs_ranking_weekly_button    vs_ranking_alliance  0.790     1.000     RECROP
```

**Every anchor in the VS tree except `daily_tab` is non-discriminative**, and the VS tree is exactly what M4 parses.

CLAUDE.md's rule that "a RECROP verdict on a screen with no false positives is declinable" does not apply here, and the matrix is what settles it: `vs_ranking_alliance` is misread as `vs_ranking` on one frame of 576. The tree does over-claim, and the separation report explains why nothing is left to stop it. That is the failure `screens.go` describes in the abstract — recognizing "some ranking screen" and parsing it feeds M4 numbers off whichever view happened to be showing, and provenance cannot rescue a genuine screenshot of the wrong screen.

Recrop each identifying anchor to carry content unique to its own screen. The discriminators available from the recon frames:

| screen | unique content |
|---|---|
| `vs_ranking` | the weekday tab strip (`Mon. Tues. Wed. …`), which Weekly does not have |
| `vs_ranking_weekly` | the selected "Weekly Rank" tab **plus** rows carrying two alliance tags |
| `vs_ranking_alliance` | the selected "Weekly Rank" tab **plus** the checked Your Alliance control |

Note that `vs_ranking_weekly` and `vs_ranking_alliance` genuinely differ only by the filter state, which is the case CLAUDE.md says not to solve by cropping the empty state. Anchor `vs_ranking_alliance` on the **checked** control — the state that has something in it — and give `vs_ranking_weekly` a discriminator that is not the absence of a checkmark.

- [ ] **Step 5: Reindex and push the corpus**

```bash
./bin/agent corpus index && ./bin/agent corpus push
```

- [ ] **Step 6: Add the screen constant**

In `internal/vision/screens.go`, add to the const block and to `ScreenNames`:

```go
	ScreenAllianceDuel       = "alliance_duel"
```

and extend the doc comment where it explains the VS tree:

```go
// VS ranking: base -> alliance_duel -> vs_ranking -> "weekly ranking" tab ->
// select your alliance. base's VS button lands on the Alliance Duel screen,
// not on a ranking screen, and the route taps through it, so it is named. It
// is NOT reachable from Alliance; docs that describe the route as
// "Alliance -> Members -> VS Ranking" are wrong.
```

- [ ] **Step 7: Run the screen-vocabulary tests**

Run: `go test ./internal/vision/ -run TestScreenNames -v`
Expected: PASS — `TestScreenNamesContainsEveryDeclaredConstant` fails if the constant was added without the slice entry.

- [ ] **Step 8: Add the graph edges**

In `internal/runtime/graph.go`, inside `DefaultGraph()`:

```go
			{From: vision.ScreenAlliance, To: vision.ScreenAllianceMembers, Action: ActionTap, AnchorID: "members_button"},
			{From: vision.ScreenAllianceMembers, To: vision.ScreenAlliance, Action: ActionBack},

			{From: vision.ScreenBase, To: vision.ScreenAllianceDuel, Action: ActionTap, AnchorID: "vs_button"},
			{From: vision.ScreenAllianceDuel, To: vision.ScreenBase, Action: ActionBack},
			{From: vision.ScreenAllianceDuel, To: vision.ScreenVSRanking, Action: ActionTap, AnchorID: "ranking_button"},
			{From: vision.ScreenVSRanking, To: vision.ScreenAllianceDuel, Action: ActionBack},
			{From: vision.ScreenVSRanking, To: vision.ScreenVSRankingWeekly, Action: ActionTap, AnchorID: "weekly_tab"},
			{From: vision.ScreenVSRankingWeekly, To: vision.ScreenVSRanking, Action: ActionBack},
			{From: vision.ScreenVSRankingWeekly, To: vision.ScreenVSRankingAlliance, Action: ActionTap, AnchorID: "vs_ranking_alliance_button"},
			{From: vision.ScreenVSRankingAlliance, To: vision.ScreenVSRankingWeekly, Action: ActionBack},
```

Update the closing comment, which currently reads "alliance_members and the vs tree stay unrouted: they are recognized for M4's benefit, and navigating them is M4's problem." That is no longer true — replace it with a note that M4 routed them.

- [ ] **Step 9: Run the graph tests**

Run: `go test ./internal/runtime/ -v`
Expected: PASS. Graph validation refuses edges naming anchors that do not exist, so a missed crop fails here.

- [ ] **Step 10: Re-run the M1 gate at seventeen screens**

Run: `make gate 2>&1 | tee /tmp/gate-m4-anchors.txt`
Expected: PASS at ≥98%.

Read the whole output, not the tail. If accuracy dropped, `./bin/agent score --json | jq '.predictions[] | select(.Label != .Predicted)'` names the frames. Remember that a low `worst-in` is a hypothesis about *either* the anchor or the frames and the report cannot tell you which — check the frames first, they are cheaper to inspect than a crop is to redo.

- [ ] **Step 11: Confirm the VS confusion is gone specifically**

The baseline before this task is `accuracy 0.9931 (572/576)` with one `vs_ranking_alliance → vs_ranking` cell. Overall accuracy is **not** the check here: one frame in 576 moves it by 0.0017, which is noise against a 0.98 gate, so the recrop could fail entirely and the gate would still pass.

Run: `./bin/agent score | head -25`
Expected: the `vs_ranking_alliance` row shows **all 56 frames in its own column** and the `vs_ranking` cell is empty.

Do not accept "the gate passes" as evidence for this step. The gate is an aggregate and this is a specific cell; that is the same distinction between the matrix and `--json` that CLAUDE.md draws for localizing a failure.

Leave the `base` anchor alone. Its `worst-in 0.131` and two `<none>` misses are the documented case where five frames caught the bottom nav bar mid-animation and had no buttons in them at all — three recrops already chased that and moved nothing.

- [ ] **Step 11: Commit**

```bash
git add internal/vision/screens.go internal/runtime/graph.go templates/ fixtures/corpus/index.yaml
git commit -m "Route the roster and the VS tree, and name the duel screen

base's VS button lands on Alliance Duel, not on a ranking screen, and the route
taps through it, so it is a seventeenth screen rather than an unnamed waypoint.
A labelled screen with no identifying anchor is scored wrong on every run
forever, so it arrives with corpus frames and an anchor rather than as a bare
constant.

vs_ranking_alliance_button is recropped to carry its Your Alliance label. As an
empty checkbox it measured stddev 2,346 -- about 11x flatter than a text anchor
-- and read worst-in 1.000 / best-out 1.000. CLAUDE.md treats that as a scoring
problem, which it is until something taps it: NCC lands its best match on an
arbitrary smooth region, and the tap goes there with invariant #3 satisfied.

weekly_tab is cropped fresh on vs_ranking because the existing anchors of that
name sit on the two screens below, where the tab is selected and looks
different."
```

---

### Task 7: The scroll-and-capture helper

**Files:**
- Create: `internal/tasks/scrollcapture.go`
- Test: `internal/tasks/scrollcapture_test.go`

**Interfaces:**
- Consumes: `vision.ScrollOffset`, `rt.Capture`, `rt.Swipe`, `rt.Sleep`, `rt.CurrentScreen`
- Produces:
  - `type ScrollSpec struct { Screen string; Region transport.Rect; Pitch int; GroupKey string }`
  - `type ScrolledFrame struct { ScreenshotID int64; Seq int; OffsetPx int; GroupKey string }`
  - `func scrollCapture(ctx context.Context, rt *runtime.Ctx, spec ScrollSpec) ([]ScrolledFrame, bool, error)` — the bool is `complete`: true when the list bottom was proven
  - `var ErrScrollOvershot = errors.New("tasks: scroll moved further than the visible region")`

- [ ] **Step 1: Write the failing test**

Create `internal/tasks/scrollcapture_test.go`. Use `runtimetest` and `transport.ReplayTransport` in the way the existing task tests do — read `internal/tasks/radar_test.go` first for the established harness shape.

```go
package tasks

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

// The list bottom must be proven, not assumed. A swallowed swipe and a real
// bottom both produce a zero offset, so the loop retries before believing it,
// exactly as startExecution retries a tap the game ignored.
func TestScrollCaptureRetriesBeforeBelievingTheBottom(t *testing.T) {
	rt, tr := newScrollHarness(t, []frameScript{
		{shift: 40}, // moved
		{shift: 0},  // swallowed swipe
		{shift: 40}, // moved after the retry
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
```

Write `newScrollHarness` and `frameScript` in the same file: `frameScript{shift int}` generates striped frames offset by the cumulative shift, and the harness feeds them through `ReplayTransport` with a `Capturer` that records screenshot IDs. Model it on the existing helpers in `internal/tasks/main_test.go`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tasks/ -run TestScrollCapture -v`
Expected: FAIL — `undefined: scrollCapture`

- [ ] **Step 3: Write the implementation**

Create `internal/tasks/scrollcapture.go`:

```go
package tasks

import (
	"context"
	"errors"
	"fmt"
	"image"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrScrollOvershot reports that the list moved further than the visible
// region between two frames, so rows passed by without ever being
// photographed. No dedupe can recover them, which is why this is an error and
// not a warning.
var ErrScrollOvershot = errors.New("tasks: scroll moved further than the visible region")

const (
	// maxScrollFrames caps a single list. R3 holds 64 members at roughly four
	// rows a swipe, so ~16 frames; 40 is comfortably double and turns a
	// non-converging loop into a fast failure rather than a hang.
	maxScrollFrames = 40
	// zeroOffsetRetries is how many times a zero offset is retried before the
	// bottom is believed. Three, matching startExecution, and for the same
	// reason: a swallowed gesture and a real end look identical.
	zeroOffsetRetries = 3
	// swipeSettle is the pause after a swipe, before capturing. Fling has to
	// finish or the offset measures a moving list.
	swipeSettleMin = 900 * time.Millisecond
	swipeSettleMax = 1400 * time.Millisecond
)

// ScrollSpec describes one scrollable list.
type ScrollSpec struct {
	// Screen is the recognized screen this list lives on. Every frame is
	// verified against it, so a mid-scroll navigation away fails rather than
	// silently capturing something else.
	Screen string
	// Region is the scrollable area, excluding sticky headers above it and any
	// pinned row below it.
	Region transport.Rect
	// Pitch is the expected row height in pixels at the reference resolution.
	Pitch int
	// GroupKey labels every frame, carrying the rank group on the roster route.
	GroupKey string
}

// ScrolledFrame is one captured frame and the offset that reached it.
type ScrolledFrame struct {
	ScreenshotID int64
	Seq          int
	OffsetPx     int
	GroupKey     string
}

// scrollCapture swipes through a list, capturing a frame per step and
// measuring how far the content actually moved. It returns the frames and
// whether the bottom was proven.
//
// The measurement is the point. Recon found fling roughly doubles a gesture,
// so a loop that trusts its swipe distance skips rows while every frame still
// looks valid — the failure is invisible downstream.
func scrollCapture(ctx context.Context, rt *runtime.Ctx, spec ScrollSpec) ([]ScrolledFrame, bool, error) {
	usable, err := usableHeight(rt, spec)
	if err != nil {
		return nil, false, err
	}

	var frames []ScrolledFrame
	prev, err := captureFrame(ctx, rt, spec, 0, 0, &frames)
	if err != nil {
		return nil, false, err
	}

	zeroes := 0
	for seq := 1; seq < maxScrollFrames; seq++ {
		if err := rt.CheckKillSwitch(ctx); err != nil {
			return frames, false, err
		}
		if err := swipeOnce(ctx, rt, spec); err != nil {
			return frames, false, err
		}

		cur, err := rt.Screenshot(ctx)
		if err != nil {
			return frames, false, fmt.Errorf("tasks: capturing scroll frame %d: %w", seq, err)
		}
		offset, err := vision.ScrollOffset(prev, cur, spec.Region)
		if err != nil {
			return frames, false, fmt.Errorf("tasks: measuring scroll frame %d on %s: %w", seq, spec.Screen, err)
		}

		switch {
		case offset == 0:
			zeroes++
			if zeroes >= zeroOffsetRetries {
				return frames, true, nil
			}
			continue
		case offset > usable:
			return frames, false, fmt.Errorf("tasks: %s moved %d px against a usable region of %d px: %w",
				spec.Screen, offset, usable, ErrScrollOvershot)
		}
		zeroes = 0

		if _, err := captureFrame(ctx, rt, spec, seq, offset, &frames); err != nil {
			return frames, false, err
		}
		prev = cur
	}
	// Ran out of frames without a proven bottom.
	return frames, false, nil
}

// usableHeight is the region's height less one row pitch: the furthest the
// list may travel while still leaving every row photographed somewhere.
func usableHeight(rt *runtime.Ctx, spec ScrollSpec) (int, error) {
	size := rt.Resolution()
	h := int((spec.Region.Y2 - spec.Region.Y1) * float64(size.Y))
	if h <= spec.Pitch {
		return 0, fmt.Errorf("tasks: region on %s is %d px, not taller than one %d px row", spec.Screen, h, spec.Pitch)
	}
	return h - spec.Pitch, nil
}

// swipeOnce performs one measured-size swipe inside the region. 300px over
// 800ms was measured on the handset at ~512px of travel — about 48% overlap
// against a 990px viewport — where 700px over 300ms travelled ~1504px and
// skipped rows.
func swipeOnce(ctx context.Context, rt *runtime.Ctx, spec ScrollSpec) error {
	midX := (spec.Region.X1 + spec.Region.X2) / 2
	span := spec.Region.Y2 - spec.Region.Y1
	from := transport.Norm{X: midX, Y: spec.Region.Y1 + span*0.75}
	to := transport.Norm{X: midX, Y: spec.Region.Y1 + span*0.40}
	if err := rt.Swipe(ctx, from, to); err != nil {
		return fmt.Errorf("tasks: swiping %s: %w", spec.Screen, err)
	}
	return rt.Sleep(ctx, swipeSettleMin, swipeSettleMax)
}

// captureFrame verifies the screen and stores one frame.
func captureFrame(ctx context.Context, rt *runtime.Ctx, spec ScrollSpec, seq, offset int, out *[]ScrolledFrame) (image.Image, error) {
	rec, err := rt.CurrentScreen(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks: recognizing before scroll frame %d: %w", seq, err)
	}
	if rec.Screen != spec.Screen {
		return nil, fmt.Errorf("tasks: expected %s at scroll frame %d, found %s", spec.Screen, seq, rec.Screen)
	}
	id, err := rt.Capture(ctx, spec.Screen)
	if err != nil {
		return nil, fmt.Errorf("tasks: storing scroll frame %d: %w", seq, err)
	}
	*out = append(*out, ScrolledFrame{ScreenshotID: id, Seq: seq, OffsetPx: offset, GroupKey: spec.GroupKey})
	return rt.Screenshot(ctx)
}
```

**Note for the implementer:** this needs `rt.Screenshot`, `rt.Resolution` and `rt.CheckKillSwitch` on `runtime.Ctx`. Check whether they exist:

Run: `grep -n "func (c \*Ctx)" internal/runtime/*.go`

If any is missing, add it in this task as a thin accessor over the existing `tr` / `ks` fields, with its own unit test. Do not reach into `Ctx`'s unexported fields from `internal/tasks`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/tasks/ -run TestScrollCapture -v`
Expected: PASS

- [ ] **Step 5: Run the whole task suite**

Run: `go test ./internal/tasks/`
Expected: `ok`

- [ ] **Step 6: Commit**

```bash
git add internal/tasks/scrollcapture.go internal/tasks/scrollcapture_test.go internal/runtime/
git commit -m "Scroll a list by measuring how far it actually moved

A swipe returning nil means the gesture was dispatched, not that the list
moved, which is the same defect the radar tap had. So the loop measures the
offset between consecutive frames and treats a zero as a hypothesis: three
retries before the bottom is believed, because a swallowed gesture and a real
end produce identical evidence.

An offset larger than the usable region is an error rather than a warning.
Rows travelled past without ever being photographed and no dedupe can recover
them. Recon proved this is not a defensive edge case: 700px over 300ms moved
~1504px against a ~990px viewport, which is what an obvious implementation
would do, and it skips about a third of the list while every frame still looks
valid. 300px over 800ms travels ~512px, leaving ~48% overlap.

Every frame re-verifies the screen, so navigating away mid-scroll fails
instead of quietly capturing something else."
```

---

### Task 8: `roster_capture`

**Files:**
- Create: `internal/tasks/roster_capture.go`
- Test: `internal/tasks/roster_capture_test.go`

**Interfaces:**
- Consumes: `scrollCapture`, `ScrollSpec`, `runtime.Ctx`
- Produces: registers task `roster_capture`; `var ErrGroupDidNotExpand = errors.New("tasks: rank group did not expand")`

- [ ] **Step 1: Write the failing test**

```go
package tasks

import (
	"context"
	"errors"
	"testing"
)

// Tapping a group header that does not expand yields a perfectly valid capture
// of the wrong group, which is the scroll-loop equivalent of the swallowed
// radar tap. It must be caught at the tap, not discovered in the data.
func TestRosterCaptureFailsWhenAGroupDoesNotExpand(t *testing.T) {
	rt := newRosterHarness(t, rosterScript{expandSucceeds: false})

	fn, ok := Get("roster_capture")
	if !ok {
		t.Fatal("roster_capture is not registered")
	}
	if err := fn(context.Background(), rt); !errors.Is(err, ErrGroupDidNotExpand) {
		t.Fatalf("got %v, want ErrGroupDidNotExpand", err)
	}
}

func TestRosterCaptureVisitsEveryRankGroup(t *testing.T) {
	rt := newRosterHarness(t, rosterScript{expandSucceeds: true})

	fn, _ := Get("roster_capture")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("roster_capture: %v", err)
	}
	// Four list groups: R4, R3, R2, R1. R5 comes from the president card.
	if got := len(rankGroupsVisited(rt)); got != 4 {
		t.Errorf("visited %d groups, want 4", got)
	}
}
```

Write `newRosterHarness`, `rosterScript` and `rankGroupsVisited` alongside, modelled on the existing task-test harnesses.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tasks/ -run TestRosterCapture -v`
Expected: FAIL — task not registered

- [ ] **Step 3: Write the implementation**

Create `internal/tasks/roster_capture.go`:

```go
package tasks

import (
	"context"
	"errors"
	"fmt"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrGroupDidNotExpand reports that a rank group header was tapped and stayed
// collapsed. Proceeding would capture whichever group happened to be open,
// which parses cleanly and is wrong.
var ErrGroupDidNotExpand = errors.New("tasks: rank group did not expand")

// rankGroups are the four collapsible groups in the member list, outermost
// first. R5 is not here: the president is shown on a card above the list, not
// as a row, so the roster route reads it from the alliance frame instead.
var rankGroups = []string{"R4", "R3", "R2", "R1"}

// memberListRegion is the scrollable part of alliance_members: below the
// sticky header block (title, president card, officer cards, search box) and
// above the panel's bottom edge.
var memberListRegion = transport.Rect{X1: 0.03, Y1: 0.42, X2: 0.97, Y2: 0.89}

// memberRowPitch is the member row height measured on the handset.
const memberRowPitch = 112

func init() { Register("roster_capture", rosterCapture) }

// rosterCapture walks Alliance → Members and captures every rank group.
//
// The member list is not one scrollable list. It is five rank groups, four of
// them collapsible, and only one is expanded at a time. Each group's header
// states its own total and those totals sum to the alliance member count, so
// reconciliation is per group rather than global — a shortfall localizes
// instead of leaving one number to explain.
func rosterCapture(ctx context.Context, rt *runtime.Ctx) error {
	var all []ScrolledFrame
	allComplete := true

	if err := rt.NavigateTo(ctx, vision.ScreenAlliance); err != nil {
		return fmt.Errorf("tasks: navigating to alliance: %w", err)
	}
	// The alliance frame carries "Members: 96/100" plus the tag, name and
	// leader. It is the reconciliation ground truth and a single stable read,
	// so it is captured before anything scrolls.
	if _, err := rt.Capture(ctx, vision.ScreenAlliance); err != nil {
		return fmt.Errorf("tasks: capturing the alliance frame: %w", err)
	}

	if err := rt.NavigateTo(ctx, vision.ScreenAllianceMembers); err != nil {
		return fmt.Errorf("tasks: navigating to the member list: %w", err)
	}

	for _, group := range rankGroups {
		if err := rt.CheckKillSwitch(ctx); err != nil {
			return err
		}
		if err := expandGroup(ctx, rt, group); err != nil {
			return err
		}
		spec := ScrollSpec{
			Screen:   vision.ScreenAllianceMembers,
			Region:   memberListRegion,
			Pitch:    memberRowPitch,
			GroupKey: group,
		}
		frames, complete, err := scrollCapture(ctx, rt, spec)
		if err != nil {
			return fmt.Errorf("tasks: capturing rank group %s: %w", group, err)
		}
		// One capture row per run, not per group. Every frame carries its
		// group in GroupKey, which is what lets ingest reconcile each group
		// against its own header total and then sum those against the member
		// count from the alliance frame -- both checks inside one capture.
		all = append(all, frames...)
		if !complete {
			allComplete = false
		}
		if err := collapseGroup(ctx, rt, group); err != nil {
			return err
		}
	}
	// A run is complete only if every group proved its own bottom. One short
	// group makes the whole roster short, and reconciliation must see that.
	return recordFrames(ctx, rt, "roster", all, allComplete)
}

// expandGroup taps a group header and confirms it opened. The chevron flips
// from ▲ to ▼, and that flip is the only local proof the tap was taken —
// exactly the confirmation startExecution needed for Quick Execute.
func expandGroup(ctx context.Context, rt *runtime.Ctx, group string) error {
	collapsed := "group_" + group + "_collapsed"
	expanded := "group_" + group + "_expanded"

	if open, err := rt.Sees(ctx, vision.ScreenAllianceMembers, expanded); err != nil {
		return fmt.Errorf("tasks: checking whether %s is open: %w", group, err)
	} else if open {
		return nil
	}
	if err := rt.Tap(ctx, vision.ScreenAllianceMembers, collapsed); err != nil {
		return fmt.Errorf("tasks: tapping the %s header: %w", group, err)
	}
	if err := rt.Sleep(ctx, swipeSettleMin, swipeSettleMax); err != nil {
		return err
	}
	open, err := rt.Sees(ctx, vision.ScreenAllianceMembers, expanded)
	if err != nil {
		return fmt.Errorf("tasks: confirming %s expanded: %w", group, err)
	}
	if !open {
		return fmt.Errorf("tasks: %s stayed collapsed after a tap: %w", group, ErrGroupDidNotExpand)
	}
	return nil
}

func collapseGroup(ctx context.Context, rt *runtime.Ctx, group string) error {
	expanded := "group_" + group + "_expanded"
	if open, err := rt.Sees(ctx, vision.ScreenAllianceMembers, expanded); err != nil || !open {
		return err
	}
	// A failure to collapse is not fatal: the next group's expand confirms its
	// own state, and leaving one open costs a scroll, not correctness.
	if err := rt.Tap(ctx, vision.ScreenAllianceMembers, expanded); err != nil {
		rt.Logger().Warn("could not collapse rank group", "group", group, "err", err)
	}
	return nil
}
```

`recordFrames` is shared with Task 9. Write it here in full:

```go
// recordFrames persists one route run as a capture plus its frames. Each frame
// keeps the offset that reached it, so ingest never has to re-measure —
// re-measurement under a changed ScrollOffset would re-segment historical
// captures into different rows and make old facts unreproducible.
//
// There is one capture per run, not per rank group. The group travels on each
// frame instead, which is what lets ingest reconcile a group against its own
// header total and then sum those against the alliance member count without
// needing a notion of a capture set.
func recordFrames(ctx context.Context, rt *runtime.Ctx, route string, frames []ScrolledFrame, complete bool) error {
	refs := make([]runtime.CaptureFrameRef, 0, len(frames))
	for _, f := range frames {
		refs = append(refs, runtime.CaptureFrameRef{
			ScreenshotID: f.ScreenshotID,
			Seq:          f.Seq,
			OffsetPx:     f.OffsetPx,
			GroupKey:     f.GroupKey,
		})
	}
	return rt.RecordCapture(ctx, route, refs, complete)
}
```

Note the `Seq` values: `scrollCapture` numbers frames from zero within each
group, so across four groups the sequence repeats. `capture_frames` has
`UNIQUE (capture_id, seq)`, which that violates. **Renumber `Seq` sequentially
across the whole run** as the refs are built, and keep the group in `GroupKey`
where it belongs.

- [ ] **Step 4: Add the group-header anchors**

`expandGroup` names eight anchors: `group_R4_collapsed` / `group_R4_expanded` and the same for R3, R2, R1. Crop them in studio against corpus `alliance_members` frames.

Both states are needed because **the collapsed chevron and the expanded chevron are the discriminator**. Do not crop the chevron alone — it is a small glyph and the flatness trap applies. Include the rank badge and group name to the left, which gives the template text variance and pins it to the right row.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/tasks/ -run TestRosterCapture -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/tasks/roster_capture.go internal/tasks/roster_capture_test.go templates/
git commit -m "Capture the roster, one rank group at a time

The member list is five rank groups rather than one scrollable list, four of
them collapsible and only one open at a time, so the route expands and scrolls
each in turn. The alliance frame is captured first because the member count
lives there, not on the member list, and it is the reconciliation ground truth.

Expanding is confirmed rather than assumed. A tapped header that stays
collapsed yields a perfectly valid capture of whichever group was already open,
which parses cleanly and is wrong -- the same shape as the radar tap the game
ignored. The chevron flip is the only local proof available.

The group anchors deliberately include the rank badge and group name rather
than just the chevron: a bare glyph is small and flat, and NCC divides out
template variance, so it would match anywhere."
```

---

### Task 9: `vs_capture`

**Files:**
- Create: `internal/tasks/vs_capture.go`
- Test: `internal/tasks/vs_capture_test.go`

**Interfaces:**
- Consumes: `scrollCapture`, `recordFrames`
- Produces: registers task `vs_capture`; `var ErrFilterNotApplied = errors.New("tasks: the Your Alliance filter did not apply")`

- [ ] **Step 1: Write the failing test**

```go
package tasks

import (
	"context"
	"errors"
	"testing"
)

// Without the filter the ranking lists both alliances, so every enemy row
// would be parsed and fail to match, flooding review with rows that are not
// ours. The checkmark is the proof the tap landed.
func TestVSCaptureFailsWhenTheAllianceFilterDoesNotApply(t *testing.T) {
	rt := newVSHarness(t, vsScript{filterApplies: false})

	fn, ok := Get("vs_capture")
	if !ok {
		t.Fatal("vs_capture is not registered")
	}
	if err := fn(context.Background(), rt); !errors.Is(err, ErrFilterNotApplied) {
		t.Fatalf("got %v, want ErrFilterNotApplied", err)
	}
}

func TestVSCaptureRecordsAnIncompleteScrollAsPartial(t *testing.T) {
	// The bottom is never proven. The capture must not be marked complete,
	// because ingest reads absence as a zero score only on a complete capture.
	rt := newVSHarness(t, vsScript{filterApplies: true, neverReachesBottom: true})

	fn, _ := Get("vs_capture")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("vs_capture: %v", err)
	}
	if status := lastCaptureStatus(rt); status != "partial" {
		t.Errorf("status = %q, want partial", status)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tasks/ -run TestVSCapture -v`
Expected: FAIL — task not registered

- [ ] **Step 3: Write the implementation**

Create `internal/tasks/vs_capture.go`:

```go
package tasks

import (
	"context"
	"errors"
	"fmt"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrFilterNotApplied reports that the Your Alliance checkbox was tapped and
// the list still shows both alliances.
var ErrFilterNotApplied = errors.New("tasks: the Your Alliance filter did not apply")

// vsListRegion is the ranking's scrollable area: below the column header and
// above the pinned self row. The pinned row is excluded deliberately — it sits
// outside the scroll region and also appears in its natural position in the
// list, so including it would photograph one member twice at two different
// screen positions.
var vsListRegion = transport.Rect{X1: 0.03, Y1: 0.185, X2: 0.97, Y2: 0.80}

// vsRowPitch is the ranking row height measured on the handset. It differs
// from the roster's, which is why pitch is a per-list parameter.
const vsRowPitch = 128

func init() { Register("vs_capture", vsCapture) }

// vsCapture walks base → alliance_duel → ranking → weekly → your alliance and
// captures the whole list.
//
// Weekly and Daily have different layouts — Daily carries a weekday tab strip
// that Weekly does not — so this route commits to Weekly and does not share a
// list region with the daily view.
func vsCapture(ctx context.Context, rt *runtime.Ctx) error {
	if err := rt.NavigateTo(ctx, vision.ScreenVSRankingWeekly); err != nil {
		return fmt.Errorf("tasks: navigating to the weekly ranking: %w", err)
	}
	if err := applyAllianceFilter(ctx, rt); err != nil {
		return err
	}

	spec := ScrollSpec{
		Screen: vision.ScreenVSRankingAlliance,
		Region: vsListRegion,
		Pitch:  vsRowPitch,
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

// applyAllianceFilter checks Your Alliance and confirms the checkmark, which
// is the only local proof the tap was taken. Without the filter the list
// carries both alliances and every enemy row would fail to match, flooding
// review with rows that were never ours.
func applyAllianceFilter(ctx context.Context, rt *runtime.Ctx) error {
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
	rec, err := rt.CurrentScreen(ctx)
	if err != nil {
		return fmt.Errorf("tasks: confirming the alliance filter: %w", err)
	}
	if rec.Screen != vision.ScreenVSRankingAlliance {
		return fmt.Errorf("tasks: still on %s after tapping Your Alliance: %w", rec.Screen, ErrFilterNotApplied)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/tasks/ -run TestVSCapture -v`
Expected: PASS

- [ ] **Step 5: Register both routes in the scheduler catalogue**

Tasks are seeded as rows in the `tasks` table. Read the existing seeding first:

Run: `sed -n '1,40p' internal/db/migrations/00003_tasks.sql`

Then follow that pattern to add `roster_capture` and `vs_capture`, with
`enabled_for_roles = {alliance_data}` and a daily cadence. Put the inserts in a
new migration rather than editing `00005_analytics.sql`, which by then has
already been applied.

- [ ] **Step 6: Commit**

```bash
git add internal/tasks/vs_capture.go internal/tasks/vs_capture_test.go internal/db/migrations/
git commit -m "Capture the weekly VS ranking, filtered to our alliance

The route is base -> alliance_duel -> ranking -> weekly -> your alliance, and
each landing is confirmed. The Your Alliance tap is confirmed by the screen
changing rather than by the tap returning nil: without the filter the list
carries both alliances, and every enemy row would parse fine, fail to match,
and flood review with rows that were never ours.

The list region excludes the pinned self row. That row sits outside the scroll
area and also appears in its natural position in the list, so including it
would photograph one member twice at two different screen positions -- which
geometric dedupe cannot see.

A scroll that fails mid-list still persists its frames and is marked not
complete. The frames are evidence, and the incomplete flag is what stops ingest
reading an absent member as a zero score."
```

---

### Task 10: Capture persistence

**Files:**
- Create: `internal/ingest/store.go`
- Modify: `internal/runtime/capture.go` (add `RecordCapture`)
- Test: `internal/runtime/capture_test.go`

**Interfaces:**
- Produces:
  - `func (c *Ctx) RecordCapture(ctx context.Context, route, group string, frames []ScrolledFrame, complete bool) error` — but `ScrolledFrame` lives in `internal/tasks`, which would invert the dependency. **Instead** define in `internal/runtime`:
    - `type CaptureFrameRef struct { ScreenshotID int64; Seq int; OffsetPx int; GroupKey string }`
    - `type CaptureRecorder interface { RecordCapture(ctx context.Context, accountID int64, route string, frames []CaptureFrameRef, complete bool) error }`
    - `func (c *Ctx) RecordCapture(ctx context.Context, route string, frames []CaptureFrameRef, complete bool) error`
  - `internal/tasks` converts `[]ScrolledFrame` → `[]runtime.CaptureFrameRef`.

- [ ] **Step 1: Write the failing test**

```go
package runtime

import (
	"context"
	"testing"
)

type recordingRecorder struct {
	route    string
	frames   []CaptureFrameRef
	complete bool
	calls    int
}

func (r *recordingRecorder) RecordCapture(_ context.Context, _ int64, route string, frames []CaptureFrameRef, complete bool) error {
	r.route, r.frames, r.complete, r.calls = route, frames, complete, r.calls+1
	return nil
}

func TestRecordCapturePassesFramesAndCompleteness(t *testing.T) {
	rec := &recordingRecorder{}
	c := newTestCtx(t, withRecorder(rec))

	frames := []CaptureFrameRef{
		{ScreenshotID: 11, Seq: 0, OffsetPx: 0, GroupKey: "R3"},
		{ScreenshotID: 12, Seq: 1, OffsetPx: 512, GroupKey: "R3"},
	}
	if err := c.RecordCapture(context.Background(), "roster", frames, false); err != nil {
		t.Fatalf("RecordCapture: %v", err)
	}
	if rec.calls != 1 || rec.route != "roster" || rec.complete {
		t.Fatalf("recorder got route=%q complete=%v calls=%d", rec.route, rec.complete, rec.calls)
	}
	if len(rec.frames) != 2 || rec.frames[1].OffsetPx != 512 {
		t.Fatalf("frames not passed through: %+v", rec.frames)
	}
}

func TestRecordCaptureWithNoRecorderIsAnError(t *testing.T) {
	c := newTestCtx(t) // built without withRecorder

	err := c.RecordCapture(context.Background(), "roster", []CaptureFrameRef{{ScreenshotID: 1}}, true)
	if err == nil {
		t.Fatal("want an error when Ctx has no recorder, got nil")
	}
	if !strings.Contains(err.Error(), "recorder") {
		t.Errorf("error should name the missing recorder, got %q", err)
	}
}
```

This mirrors how `Capture` already behaves when `cap` is nil — read
`internal/runtime/capture.go` and follow that shape, including its error text
style.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/runtime/ -run TestRecordCapture -v`
Expected: FAIL

- [ ] **Step 3: Implement `RecordCapture` on `Ctx` and the pool-side recorder**

Add the interface and method to `internal/runtime/capture.go`, following how `Capturer` / `Capture` are already wired. Implement the interface on `*db.Pool` in `internal/db/analytics.go`:

```go
// RecordCapture writes one capture and its frames in a single transaction, so
// a killed process never leaves frames pointing at a capture row that does not
// exist. Status is derived here rather than passed as free text: only a
// capture whose scroll proved the list bottom is complete.
func (p *Pool) RecordCapture(ctx context.Context, accountID int64, route string, frames []runtime.CaptureFrameRef, complete bool) error {
	status := "partial"
	if complete {
		status = "complete"
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: beginning capture for %s: %w", route, err)
	}
	defer tx.Rollback(ctx)

	var captureID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO captures (account_id, route, status, parsed_rows, ended_at)
		VALUES ($1,$2,$3,0,now()) RETURNING id`,
		accountID, route, status).Scan(&captureID); err != nil {
		return fmt.Errorf("db: inserting capture for %s: %w", route, err)
	}
	for _, f := range frames {
		if _, err := tx.Exec(ctx, `
			INSERT INTO capture_frames (capture_id, seq, screenshot_id, offset_px, group_key)
			VALUES ($1,$2,$3,$4,$5)`,
			captureID, f.Seq, f.ScreenshotID, f.OffsetPx, nullString(f.GroupKey)); err != nil {
			return fmt.Errorf("db: inserting frame %d of capture %d: %w", f.Seq, captureID, err)
		}
	}
	return tx.Commit(ctx)
}
```

Note the import direction: `internal/db` importing `internal/runtime` may create a cycle. Check with `go build ./...`. If it does, move `CaptureFrameRef` into `internal/db` and have `internal/runtime` reference `db.CaptureFrameRef`, mirroring how `db.CaptureTarget` is already used by `internal/capture`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/runtime/ && go build ./...`
Expected: PASS, builds clean.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime internal/db
git commit -m "Persist a capture and its frames in one transaction

A killed process must never leave capture_frames rows pointing at a capture
that does not exist, so both go in one transaction. Status is derived from
whether the scroll proved the list bottom rather than passed in as free text --
partial is load-bearing downstream, because ingest reads an absent member as a
zero score only on a complete capture."
```

---

### Task 11: Roster ingest

**Files:**
- Create: `internal/ingest/roster.go`
- Test: `internal/ingest/roster_test.go`

**Interfaces:**
- Consumes: `SegmentRows`, `ParsePower`, `ParseLevel`, `ParseLastActiveHours`, `roster.Rank`, `ocr.OCREngine`
- Produces:
  - `type RosterRow struct { Name string; NameConf float64; Power int64; Level int; LastActiveHours float64; ScreenshotID int64; Band RowBand; GroupKey string }`
  - `func (i *Ingester) IngestRoster(ctx context.Context, captureID int64) (RosterResult, error)`
  - `type RosterResult struct { Matched, Created, Queued int; PerGroup map[string]GroupTally; Status string }`
  - `type GroupTally struct { Expected, Parsed int }`

- [ ] **Step 1: Write the failing test**

Key behaviours to cover, each its own test function:

```go
package ingest

import (
	"context"
	"testing"
)

// The structural guard the recon supplied: if a group states 11 members and
// eleven already matched, a twelfth is an OCR artifact rather than a person.
// This is a check where the alternative is a tuned threshold.
func TestIngestRosterRefusesToCreateBeyondTheGroupCount(t *testing.T) {
	ing := newRosterIngestHarness(t, rosterFixture{
		group:        "R2",
		groupTotal:   11,
		existing:     11,
		extraGarbled: 1,
	})

	res, err := ing.IngestRoster(context.Background(), 1)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Created != 0 {
		t.Errorf("created %d members, want 0 — the group is already full", res.Created)
	}
	if res.Queued != 1 {
		t.Errorf("queued %d for review, want 1", res.Queued)
	}
}

func TestIngestRosterMarksAShortGroupPartial(t *testing.T) {
	ing := newRosterIngestHarness(t, rosterFixture{group: "R3", groupTotal: 64, parsedRows: 60})

	res, err := ing.IngestRoster(context.Background(), 1)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Status != "partial" {
		t.Errorf("status = %q, want partial", res.Status)
	}
	if got := res.PerGroup["R3"]; got.Expected != 64 || got.Parsed != 60 {
		t.Errorf("R3 tally = %+v, want {64 60}", got)
	}
}

func TestIngestRosterQueuesALowConfidenceNameRatherThanGuessing(t *testing.T) {
	ing := newRosterIngestHarness(t, rosterFixture{
		group: "R2", groupTotal: 11, existing: 11, ambiguousName: true,
	})

	res, err := ing.IngestRoster(context.Background(), 1)
	if err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	if res.Queued == 0 {
		t.Error("an ambiguous name must reach review, never a leaderboard")
	}
}

func TestIngestRosterWritesFactsWithScreenshotProvenance(t *testing.T) {
	ing := newRosterIngestHarness(t, rosterFixture{group: "R2", groupTotal: 2, existing: 2})

	if _, err := ing.IngestRoster(context.Background(), 1); err != nil {
		t.Fatalf("IngestRoster: %v", err)
	}
	for _, f := range ing.store.Facts {
		if f.ScreenshotID == 0 {
			t.Errorf("fact %+v has no screenshot reference", f)
		}
		if f.Confidence <= 0 {
			t.Errorf("fact %+v has no confidence", f)
		}
	}
}
```

Write `newRosterIngestHarness` with a fake store (implementing the `Store` interface from Task 12) and `ocr.FakeEngine` scripted results. The fake store records facts and review items in slices.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/ingest/ -run TestIngestRoster -v`
Expected: FAIL

- [ ] **Step 3: Implement**

Create `internal/ingest/roster.go`. Core loop, per frame, per band:

1. Crop the band; crop each field's sub-rect within it.
2. `ocr.Read` each field with its `Spec`.
3. Parse; on `ErrUnparseable`, queue for review and continue.
4. `roster.Rank` the name against `ListMembers`.
5. `score >= roster.AutoAccept` → matched. `>= roster.ReviewFloor` → queue. Below → queue flagged reject.
6. No candidate at all **and** the group is not yet full → create. Group full → queue.
7. Write facts `power`, `level`, `last_active_hours` with `min(nameConf, fieldConf)`.
8. Tally per group; any group short of its header total ⇒ `partial`.

The group's expected total comes from OCR of the group header on the frame whose `group_key` matches — read it once per group from the first frame carrying that key.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/ingest/ -run TestIngestRoster -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/roster.go internal/ingest/roster_test.go
git commit -m "Ingest the roster into members and facts

Member creation is gated on the rank group's own stated total rather than on a
confidence threshold. If a group says 11 and eleven members already matched, a
twelfth is an OCR artifact, not a person -- a structural check where the
alternative is a tuned number. That matters because names are simultaneously
the worst OCR target on the screen and the field carrying identity, so a
mangled read does not merely fail to match, it mints a phantom that then
accumulates facts.

Reconciliation is per group, since each header states its own total and the
totals sum to the alliance member count. A shortfall localizes to one group
instead of leaving a single global discrepancy to explain.

Every fact carries the screenshot it came from and a confidence that is the
minimum of the name match and the field read: a name matched at 0.95 whose
power read at 0.6 is not a 0.95 fact."
```

---

### Task 12: VS ingest

**Files:**
- Create: `internal/ingest/vs.go`, `internal/ingest/store.go`, `internal/ingest/ingest.go`
- Test: `internal/ingest/vs_test.go`

**Interfaces:**
- Produces:
  - `type Store interface { ... }` — the database surface ingest needs, so tests use a fake
  - `type Ingester struct { ... }`, `func New(store Store, blobs blob.Store, engine ocr.OCREngine) *Ingester`
  - `func (i *Ingester) IngestVS(ctx context.Context, captureID int64, periodKey string) (VSResult, error)`
  - `type VSResult struct { Matched, Queued, Zeroed int; Status string }`

- [ ] **Step 1: Write the failing test**

```go
package ingest

import (
	"context"
	"testing"
)

// The rule the recon forced: the ranking lists scorers only, so an absent
// member scored zero -- but only if the capture is known to have reached the
// bottom. On a partial capture absence and truncation are indistinguishable.
func TestIngestVSWritesZeroesOnlyForACompleteCapture(t *testing.T) {
	ing := newVSIngestHarness(t, vsFixture{
		captureComplete: true,
		rosterSize:      96,
		rankedRows:      94,
	})

	res, err := ing.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Zeroed != 2 {
		t.Errorf("zeroed %d, want 2 — 96 members less 94 ranked", res.Zeroed)
	}
}

func TestIngestVSWritesNoZeroesOnAPartialCapture(t *testing.T) {
	ing := newVSIngestHarness(t, vsFixture{
		captureComplete: false,
		rosterSize:      96,
		rankedRows:      40,
	})

	res, err := ing.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Zeroed != 0 {
		t.Fatalf("zeroed %d on a partial capture, want 0 — absence and truncation are indistinguishable there", res.Zeroed)
	}
	if res.Status != "partial" {
		t.Errorf("status = %q, want partial", res.Status)
	}
}

// A VS row that matches nothing must never create a member: the roster route
// is the only writer of that table.
func TestIngestVSNeverCreatesAMember(t *testing.T) {
	ing := newVSIngestHarness(t, vsFixture{captureComplete: true, rosterSize: 2, rankedRows: 3})

	if _, err := ing.IngestVS(context.Background(), 1, "2026-W33"); err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if ing.store.MembersCreated != 0 {
		t.Errorf("created %d members, want 0", ing.store.MembersCreated)
	}
	if len(ing.store.Reviews) == 0 {
		t.Error("the unmatched row must have reached review")
	}
}

func TestIngestVSDeduplicatesThePinnedSelfRow(t *testing.T) {
	// The logged-in account appears pinned and in place. Geometric dedupe
	// cannot see that, because the two sit at different screen positions.
	ing := newVSIngestHarness(t, vsFixture{
		captureComplete: true, rosterSize: 5, rankedRows: 5, duplicateSelfRow: true,
	})

	res, err := ing.IngestVS(context.Background(), 1, "2026-W33")
	if err != nil {
		t.Fatalf("IngestVS: %v", err)
	}
	if res.Matched != 5 {
		t.Errorf("matched %d, want 5 — the pinned self row is the same member twice", res.Matched)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/ingest/ -run TestIngestVS -v`
Expected: FAIL

- [ ] **Step 3: Write `store.go`**

```go
package ingest

import (
	"context"

	"github.com/tomharris/lw-manager/internal/db"
)

// Store is the slice of the database ingest needs. It is an interface so the
// tests run against a fake with no Postgres, per the replay-before-real
// discipline the project follows everywhere. *db.Pool satisfies it.
type Store interface {
	Capture(ctx context.Context, id int64) (db.Capture, error)
	CaptureFrames(ctx context.Context, captureID int64) ([]db.CaptureFrame, error)
	ScreenshotObjectKey(ctx context.Context, screenshotID int64) (string, error)
	ListMembers(ctx context.Context, allianceID int64) ([]db.Member, error)
	MemberAliases(ctx context.Context, allianceID int64) (map[int64][]string, error)
	CreateMember(ctx context.Context, m db.Member) (int64, error)
	InsertFact(ctx context.Context, f db.Fact) (int64, error)
	QueueReview(ctx context.Context, r db.ReviewItem) (int64, error)
	FinishCapture(ctx context.Context, id int64, status string, parsed int, errMsg string) error
	CurrentAllianceID(ctx context.Context) (int64, error)
}
```

- [ ] **Step 4: Write `vs.go`**

Per frame, per new band: crop rank / name / points, OCR, parse, match. Then:

```go
	// The ranking lists only members with a nonzero score -- recon measured 94
	// ranked rows against 96 members. So a member absent from the capture
	// scored zero. That inference is only sound if the capture reached the
	// bottom of the list: on a partial capture, absence and truncation look
	// identical, and writing zeroes would silently under-report exactly the
	// people who are hardest to see.
	if capture.Status == "complete" {
		for _, m := range members {
			if _, seen := scored[m.ID]; seen {
				continue
			}
			if _, err := i.store.InsertFact(ctx, db.Fact{
				MemberID: m.ID, Metric: "vs_points", Value: 0,
				ObservedAt: observedAt, PeriodKey: periodKey,
				Source: "ocr:vs_ranking", ScreenshotID: lastFrameShotID,
				Confidence: zeroInferenceConfidence,
			}); err != nil {
				return VSResult{}, fmt.Errorf("ingest: writing an inferred zero for member %d: %w", m.ID, err)
			}
			res.Zeroed++
		}
	}
```

`zeroInferenceConfidence` is a named constant, not `1.0`: an inferred zero is weaker evidence than a read number, and the leaderboard should be able to tell them apart. Set it to `0.90` and document why.

Deduplicate by member ID before writing, which is what collapses the pinned self row.

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/ingest/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/ingest/vs.go internal/ingest/store.go internal/ingest/ingest.go internal/ingest/vs_test.go
git commit -m "Ingest the weekly ranking, inferring zeroes only when the list ended

The ranking lists scorers only -- 94 ranked rows against 96 members -- so a
member absent from a capture scored nothing. That inference is sound only if
the capture reached the bottom of the list: on a partial capture, absence and
truncation are indistinguishable, and writing zeroes would silently
under-report exactly the people hardest to see. So a partial capture writes no
zeroes at all, which makes the bottom-of-list proof a correctness precondition
rather than a completeness nicety.

An inferred zero carries a lower confidence than a read number, because it is
weaker evidence and a leaderboard should be able to tell them apart.

A VS row that matches nothing reviews rather than creating a member: the roster
route is the only writer of that table. Rows are deduplicated by member id
before writing, which is what collapses the self row that appears both pinned
below the list and in its natural position inside it."
```

---

### Task 13: `control ingest`

**Files:**
- Modify: `cmd/control/main.go`
- Test: `cmd/control/ingest_test.go`

- [ ] **Step 1: Write the failing test**

Test argument parsing and that results go to **stdout** while logs go to **stderr**, matching the CLI convention.

```go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestIngestPrintsASummaryToStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runIngest(&out, &errOut, []string{"--capture", "7"}, fakeIngester{matched: 94, queued: 2, zeroed: 2})
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "94") {
		t.Errorf("stdout missing the matched count: %q", out.String())
	}
	if strings.Contains(out.String(), "level=") {
		t.Error("log output leaked into stdout — results must stay pipeable")
	}
}

func TestIngestRejectsAMissingCaptureFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runIngest(&out, &errOut, nil, fakeIngester{}); code == 0 {
		t.Fatal("want a non-zero exit when --capture is absent")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/control/ -run TestIngest -v`
Expected: FAIL

- [ ] **Step 3: Implement the subcommand**

Follow the existing subcommand shape in `cmd/control/main.go`. Flags: `--capture <id>` (required), `--period <key>` (defaults to the ISO week for VS, the date for roster). Route is read from the capture row, so one command serves both.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/control/ -v && make build`
Expected: PASS, binaries build.

- [ ] **Step 5: Commit**

```bash
git add cmd/control
git commit -m "Add control ingest

Reads the route from the capture row, so one command serves both. Results go to
stdout and logs to stderr, so the summary stays pipeable."
```

---

### Task 14: Studio review

**Files:**
- Create: `internal/studio/review.go`
- Modify: `internal/studio/studio.go` (routes), `internal/studio/views.go` (template)
- Test: `internal/studio/review_test.go`

**Interfaces:**
- Produces: `GET /review`, `POST /review/{id}/resolve`, `GET /review/{id}/crop`

- [ ] **Step 1: Write the failing test**

```go
package studio

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewListsPendingItemsWithCandidates(t *testing.T) {
	s := newReviewServer(t, []pendingItem{
		{ID: 1, RawText: "kaln445", Candidates: []candidate{{MemberID: 2, Name: "Kain445", Score: 86}}},
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/review", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "kaln445") || !strings.Contains(body, "Kain445") {
		t.Errorf("review page missing the raw text or the candidate: %q", body)
	}
}

// Resolving must write the alias, which is the mechanism that makes matching
// improve rather than needing to be re-tuned.
func TestResolveWritesAnAliasAndTheFact(t *testing.T) {
	store := &fakeReviewStore{}
	s := newReviewServerWithStore(t, store)

	req := httptest.NewRequest(http.MethodPost, "/review/1/resolve", strings.NewReader("member_id=2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(req))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if len(store.Aliases) != 1 {
		t.Errorf("aliases written = %d, want 1", len(store.Aliases))
	}
	if len(store.Facts) != 1 {
		t.Errorf("facts written = %d, want 1", len(store.Facts))
	}
}

func TestReviewRequiresTheToken(t *testing.T) {
	s := newReviewServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/review", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("unauthenticated access to /review must not succeed")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/studio/ -run TestReview -v`
Expected: FAIL

- [ ] **Step 3: Implement**

`GET /review/{id}/crop` serves the row crop by fetching the screenshot blob and cropping `row_y0..row_y1` — the reason this is a served UI and not a CLI is that the reviewer needs the pixels. Reuse the existing blob-serving path from `handleFrame`.

`studio.Options` gains an optional review store; when nil the routes are not registered, so corpus-only use of studio keeps working with no database.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/studio/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/studio
git commit -m "Review uncertain reads in studio, next to their pixels

The row crop is served beside the ranked candidates, which is the whole reason
this is a served UI rather than a CLI: the box is headless, and a review
without the pixels is not a review. Studio already solved browser-over-SSH for
corpus labelling and already serves blobs, so the alternative was a second
server duplicating its auth, templates and blob path.

Resolving writes an alias before the fact. That is the mechanism that makes
matching compound -- tomorrow's identical misread matches directly instead of
queueing again.

The review routes register only when a store is configured, so corpus-only use
of studio still runs with no database."
```

---

### Task 15: The M4 gate

**Files:**
- Create: `internal/ingest/gate_test.go` (build tag `m4gate`), `fixtures/m4gate/expected.yaml`
- Modify: `Makefile`

- [ ] **Step 1: Capture one real VS ranking**

```bash
./bin/agent run-task --account <id> --task vs_capture
./bin/control ingest --capture <id printed above>
```

- [ ] **Step 2: Hand-check the capture**

Open each frame and transcribe the true rank, name and points into `fixtures/m4gate/expected.yaml`:

```yaml
capture: 42
period_key: "2026-W33"
game_version: "1.0.357"
rows:
  - rank: 1
    name: "Lothar232"
    points: 73614570
  - rank: 2
    name: "BobLeeSwagger44"
    points: 65336176
```

This is the gate's ground truth and is the one artifact that cannot be generated. Transcribe from the screenshots, not from ingest's output — checking a pipeline against itself proves nothing.

- [ ] **Step 3: Write the gate test**

```go
//go:build m4gate

package ingest_test

// The gate has three conditions and the second is the one that distinguishes
// it from an accuracy score: a pipeline that silently drops its hard rows
// scores well on the first condition alone.
//
//  1. parsed value within 1% of the hand-checked value on >= 95% of rows
//  2. every discrepancy produced a review row -- none dropped
//  3. the capture reconciles to complete
```

Skip with a clear message when `fixtures/m4gate/expected.yaml` is absent, mirroring how the M1 gate skips an unpulled corpus.

- [ ] **Step 4: Add the Makefile target**

```makefile
gate-m4:
	CGO_ENABLED=0 go test -tags m4gate -timeout 20m ./internal/ingest/
```

- [ ] **Step 5: Run the gate**

Run: `make gate-m4`
Expected: PASS on all three conditions.

- [ ] **Step 6: Commit**

```bash
git add internal/ingest/gate_test.go fixtures/m4gate Makefile
git commit -m "Add the M4 gate

Asserts three things, and the second is what makes it a gate rather than an
accuracy score: every discrepancy must have produced a review row. A pipeline
that silently drops its hard rows scores well on accuracy alone, and silent
dropping is the failure this milestone exists to prevent.

The expected rows are transcribed from the screenshots by hand rather than from
ingest's own output, since checking a pipeline against itself proves nothing."
```

---

### Task 16: Documentation

**Files:**
- Modify: `CLAUDE.md`, `docs/lastwar-platform-design.gen`

- [ ] **Step 1: Update the Layout section of CLAUDE.md**

Add:

```
internal/ingest     capture frames -> facts; OCR, segmentation, reconciliation
internal/roster     name normalization and fuzzy matching to known members
```

- [ ] **Step 2: Correct the `stayon` claim**

The current text calls `adb shell svc power stayon true` "the load-bearing one". Recon contradicted it: the handset was found dozing behind a keyguard with `mStayOn=true`, `stay_on_while_plugged_in=15`, USB powered, battery full, and no reboot in 13 days.

Rewrite that passage to say `stayon` is necessary but **not sufficient** on this handset, that the mechanism is unknown, and that clearing the keyguard credential is therefore mandatory rather than advisory — because `Wake` turns the display on and lands on the keyguard.

- [ ] **Step 3: Add the zero-byte capture gotcha**

Add to Gotchas:

```
- **A `FLAG_SECURE` surface returns a zero-byte capture, not a black frame.**
  On the PIN entry screen `adb exec-out screencap -p` returns 0 bytes; backing
  out returns a full frame. This fails at PNG decode rather than at
  recognition, which is a different error path from a sleeping display.
- **A black frame is not proof of a sleeping display.** A frame captured
  mid-transition is also solid black while `mWakefulness=Awake`,
  `mScreenState=ON` and `mStayOn=true` all report healthy.
```

- [ ] **Step 4: Correct the design doc's route table**

`docs/lastwar-platform-design.gen` §5 lists the VS route as "Events → VS Duel → Ranking". Recon confirmed it is `base → VS → Alliance Duel → Ranking → Weekly Rank → Your Alliance`. Correct it, and note that the weekly ranking lists scorers only.

- [ ] **Step 5: Verify the full suite**

```bash
make test
make test-integration
make gate
make gate-m4
```
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add CLAUDE.md docs/lastwar-platform-design.gen
git commit -m "Correct the stayon claim and record two capture failure modes

Recon found the handset dozing behind a keyguard with mStayOn=true,
stay_on_while_plugged_in=15, USB powered, battery full and no reboot in 13
days. Every precondition CLAUDE.md names was satisfied, so calling stayon the
load-bearing one is wrong on this device. The mechanism is unknown and the
correction does not guess at one; it makes clearing the keyguard credential
mandatory instead, because Wake turns the display on and lands on the keyguard,
where captures return nothing.

Also records that a FLAG_SECURE surface returns a zero-byte capture rather than
a black frame -- failing at PNG decode rather than at recognition -- and that a
black frame is also what a mid-transition capture looks like, which weakens the
inference Phase B drew from that signal."
```

---

## Self-Review

**Spec coverage.** Every section of the design doc maps to a task: §2 architecture → Tasks 10, 12, 13; §3 capture → Tasks 6, 7, 8, 9; §4 ingest → Tasks 2, 3, 4, 11, 12; §5 schema → Task 5; §6 reconciliation → Tasks 11, 12; §7 gate → Task 15; §8 testing → throughout; §9 documentation → Task 16.

**Known gap, stated rather than hidden.** The design doc's §4 "Review, in studio" is Task 14, but the *resolution* path writes a superseding fact via `SupersedeFact`, which Task 5 provides and Task 14 uses; no task exercises the full round trip of `ingest → review → resolve → superseded fact` end to end. Task 14's `TestResolveWritesAnAliasAndTheFact` covers the write, and Task 5's `TestFactsAreAppendOnlyAndSupersede` covers supersession, but the seam between them is untested. **Add an integration test to Task 14 covering that round trip** if time allows; it is the one place two verified halves could still fail to meet.

**Type consistency.** `RowBand` (Task 2) is used by Tasks 11, 12, 14. `roster.Member` (Task 4) and `db.Member` (Task 5) are deliberately distinct types — the matcher's view carries aliases inline, the database's does not — and Task 11 converts between them. `CaptureFrameRef` lives in `internal/runtime` or `internal/db` depending on the import-cycle check in Task 10 Step 3; whichever it is, Tasks 8 and 9 reference it by that path.

**Placeholder scan.** Task 7's harness helpers, Task 8's and 9's harnesses, and Tasks 11/12's fakes are described rather than written out, because each must be modelled on the existing test harnesses in `internal/tasks/main_test.go` and reproducing those here would be guessing at code the implementer can read. Every such step names the file to read first. Task 10's second test is left as a one-line stub with instructions — **that one is a genuine placeholder and should be written out by the implementer before starting.**

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-12-m4-analytics-collection.md`.
