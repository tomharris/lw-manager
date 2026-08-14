//go:build scrolldiag

package vision

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/transport"
)

var (
	ppIn  = flag.String("ppin", "", "frame to crop and preprocess")
	ppOut = flag.String("ppout", "", "where to write the preprocessed crop")
	ppY1  = flag.Float64("ppy1", 0.404, "")
	ppY2  = flag.Float64("ppy2", 0.438, "")
	ppX1  = flag.Float64("ppx1", 0.03, "")
	ppX2  = flag.Float64("ppx2", 0.97, "")
)

// TestPreprocProbe writes what the OCR engine is actually handed, so the
// preprocessing chain can be inspected rather than inferred from its output.
func TestPreprocProbe(t *testing.T) {
	if *ppIn == "" || *ppOut == "" {
		t.Skip("need -ppin and -ppout")
	}
	f, err := os.Open(*ppIn)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	out := Preprocess(img, Options{
		Region: transport.Rect{X1: *ppX1, Y1: *ppY1, X2: *ppX2, Y2: *ppY2},
	})
	w, err := os.Create(*ppOut)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := png.Encode(w, out); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s bounds=%v", *ppOut, out.Bounds())
}

// Flags for TestPreprocMeasure. Prefixed "pm" (preprocess-measure), distinct
// from TestPreprocProbe's "pp" flags and from scrolldiag_manual_test.go's
// "-frames" — both files build under the same scrolldiag tag, so a shared
// flag.String name would panic at flag registration.
//
// "|" separates list entries rather than "," because an expected VS points
// value legitimately contains a comma ("101,286,241"), which a comma-split
// would cut in half.
var (
	pmFrames  = flag.String("pmframes", "", "'|'-separated frame paths to measure")
	pmExpect  = flag.String("pmexpect", "", "'|'-separated expected substrings, aligned by index with -pmframes; an empty entry means no expectation for that frame")
	pmX1      = flag.Float64("pmx1", 0.03, "region left, fraction of frame width")
	pmY1      = flag.Float64("pmy1", 0.404, "region top, fraction of frame height")
	pmX2      = flag.Float64("pmx2", 0.97, "region right, fraction of frame width")
	pmY2      = flag.Float64("pmy2", 0.438, "region bottom, fraction of frame height")
	pmCharset = flag.String("pmcharset", "", "tesseract char whitelist, e.g. the field's ocr.Spec.Charset")
	pmUpscale = flag.String("pmupscale", "2,3,4", "comma-separated upscale factors to try per shape")
	pmPSM     = flag.Int("pmpsm", 7, "tesseract page-segmentation mode; 7 (single line) is what readField uses in production")
)

// ppShape is one combination of the three skip flags Options exposes,
// independent of upscale factor — the axis TestPreprocMeasure sweeps
// separately so a shape's own report line shows how it behaves at every
// factor, not just the one somebody guessed first.
type ppShape struct {
	name                                    string
	skipEqualize, skipThreshold, skipInvert bool
}

// ppShapes is every one of the 2×2×2 skip-flag combinations, which strictly
// contains the four the task brief calls "at minimum": full (all three
// applied), gray (all three skipped, i.e. "grayscale + upscale only"),
// gray+eq ("grayscale + equalize + upscale"), and gray+thr ("grayscale +
// threshold + upscale", isolating the step Finding 1 blamed). The other four
// are not busywork: gray+inv and gray+eq+inv test whether a field needs the
// polarity flip without the threshold that damages a flat region, and
// gray+eq+thr / gray+thr+inv bracket "full" from either side, which is what
// actually answers "is threshold the culprit, or is it threshold-plus-something".
// Order here is the order printed, full first (today's baseline) and gray
// last (Finding 1's replacement), with the rest walking equalize → threshold
// → invert in between.
var ppShapes = []ppShape{
	{name: "full", skipEqualize: false, skipThreshold: false, skipInvert: false},
	{name: "gray+eq+thr", skipEqualize: false, skipThreshold: false, skipInvert: true},
	{name: "gray+thr+inv", skipEqualize: true, skipThreshold: false, skipInvert: false},
	{name: "gray+thr", skipEqualize: true, skipThreshold: false, skipInvert: true},
	{name: "gray+eq+inv", skipEqualize: false, skipThreshold: true, skipInvert: false},
	{name: "gray+eq", skipEqualize: false, skipThreshold: true, skipInvert: true},
	{name: "gray+inv", skipEqualize: true, skipThreshold: true, skipInvert: false},
	{name: "gray", skipEqualize: true, skipThreshold: true, skipInvert: true},
}

// TestPreprocMeasure is the measurement harness the task-21 brief asked for:
// it runs every shape in ppShapes, at every upscale factor in -pmupscale,
// over every frame in -pmframes, through the real tesseract binary, and
// prints a variant × frame table of what came back — plus, when -pmexpect
// supplies a known-good substring per frame, how many of that variant's reads
// contained it. This exists so a field's Options are set from a number, not
// from eyeballing one crop: Finding 1's whole point was that a chain "tuned"
// by eye against synthetic fixtures was backwards on real ones.
//
// Needs tesseract on PATH (skips cleanly without it, same as ocr's own
// real-engine test) and does not need a device — it reads frame files
// already on disk, e.g. ones pulled from the blob store or the committed
// evidence directories under docs/superpowers/specs/evidence/.
//
// Example, reproducing the group-header measurement task-21 was built from:
//
//	go test -tags scrolldiag ./internal/vision -run TestPreprocMeasure -v -args \
//	  -pmframes "f1.png|f2.png|f3.png" \
//	  -pmexpect "This Is It||Footloose" \
//	  -pmx1 0.03 -pmy1 0.404 -pmx2 0.97 -pmy2 0.438
func TestPreprocMeasure(t *testing.T) {
	if *pmFrames == "" {
		t.Skip("need -pmframes")
	}
	eng := ocr.NewTesseractEngine()
	eng.PSM = *pmPSM
	if !eng.Available() {
		t.Skip("tesseract not installed; skipping real-engine measurement (see CLAUDE.md)")
	}

	framePaths := strings.Split(*pmFrames, "|")
	var expects []string
	if *pmExpect != "" {
		expects = strings.Split(*pmExpect, "|")
		if len(expects) != len(framePaths) {
			t.Fatalf("-pmexpect has %d entries, -pmframes has %d; they must align 1:1 (use an empty entry for 'no expectation')", len(expects), len(framePaths))
		}
	}

	imgs := make([]image.Image, len(framePaths))
	for i, p := range framePaths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatalf("opening %s: %v", p, err)
		}
		img, err := png.Decode(f)
		f.Close()
		if err != nil {
			t.Fatalf("decoding %s: %v", p, err)
		}
		imgs[i] = img
	}

	var factors []int
	for _, s := range strings.Split(*pmUpscale, ",") {
		f, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || f < 1 {
			t.Fatalf("-pmupscale %q: %q is not a positive integer", *pmUpscale, s)
		}
		factors = append(factors, f)
	}

	region := transport.Rect{X1: *pmX1, Y1: *pmY1, X2: *pmX2, Y2: *pmY2}
	spec := ocr.Spec{Charset: *pmCharset}

	type cell struct {
		text string
		conf float64
	}
	// results[variant][frame index]. variant order is preserved explicitly
	// (shapes × factors) rather than relying on map iteration, so the printed
	// table is stable across runs — a diff-able table is what makes "which
	// variant improved" answerable without re-running everything.
	type variant struct {
		name  string
		shape ppShape
		scale int
	}
	var variants []variant
	for _, shape := range ppShapes {
		for _, f := range factors {
			variants = append(variants, variant{name: fmt.Sprintf("%s@%d", shape.name, f), shape: shape, scale: f})
		}
	}

	results := make(map[string][]cell, len(variants))
	for _, v := range variants {
		row := make([]cell, len(imgs))
		for fi, img := range imgs {
			pre := Preprocess(img, Options{
				Region:        region,
				UpscaleFactor: v.scale,
				SkipEqualize:  v.shape.skipEqualize,
				SkipThreshold: v.shape.skipThreshold,
				SkipInvert:    v.shape.skipInvert,
			})
			res, err := eng.Read(context.Background(), pre, spec)
			if err != nil {
				row[fi] = cell{text: fmt.Sprintf("<error: %v>", err)}
				continue
			}
			row[fi] = cell{text: res.Text, conf: res.Confidence}
		}
		results[v.name] = row
	}

	// Print the table: one row per variant, one column per frame, plus a
	// trailing match count when expectations were supplied. t.Log rather than
	// fmt.Print so `go test -v` interleaves it correctly and a non-verbose run
	// stays quiet.
	var b strings.Builder
	fmt.Fprintf(&b, "region=%v psm=%d charset=%q frames=%d\n", region, *pmPSM, *pmCharset, len(imgs))
	for _, v := range variants {
		row := results[v.name]
		matched, total := 0, 0
		fmt.Fprintf(&b, "%-14s", v.name)
		for fi, c := range row {
			text := strings.TrimSpace(c.text)
			mark := " "
			if expects != nil && expects[fi] != "" {
				total++
				if strings.Contains(strings.ToLower(text), strings.ToLower(expects[fi])) {
					matched++
					mark = "="
				} else {
					mark = "x"
				}
			}
			fmt.Fprintf(&b, "  [%s%2d] %-30q", mark, fi, truncate(text, 30))
		}
		if total > 0 {
			fmt.Fprintf(&b, "  (%d/%d matched)", matched, total)
		}
		b.WriteByte('\n')
	}
	t.Log(b.String())

	// A second, sorted-by-match-rate summary, because the wide table above is
	// for inspecting individual frames and this is for picking a winner: the
	// number that actually justifies an Options choice in a code comment.
	if expects != nil {
		type score struct {
			name         string
			matched, tot int
		}
		var scores []score
		for _, v := range variants {
			row := results[v.name]
			m, tot := 0, 0
			for fi, c := range row {
				if expects[fi] == "" {
					continue
				}
				tot++
				if strings.Contains(strings.ToLower(strings.TrimSpace(c.text)), strings.ToLower(expects[fi])) {
					m++
				}
			}
			scores = append(scores, score{v.name, m, tot})
		}
		sort.SliceStable(scores, func(i, j int) bool { return scores[i].matched > scores[j].matched })
		var sb strings.Builder
		sb.WriteString("ranked by match count:\n")
		for _, s := range scores {
			fmt.Fprintf(&sb, "  %-14s %d/%d\n", s.name, s.matched, s.tot)
		}
		t.Log(sb.String())
	}
}

// truncate cuts s to at most n runes, marking that it was cut — the table
// above prioritizes fitting many frames on one line over showing a field's
// full raw text, which -v output still has if a wider look is needed.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
