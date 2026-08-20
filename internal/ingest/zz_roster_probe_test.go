//go:build m4probe

// The roster probe: what does the member list's name column actually read,
// and does its crop contain anything that is not the name?
//
// The VS ranking got three committed instruments (zz_name_probe_test.go,
// zz_points_probe_test.go, zz_assign_probe_test.go) and the roster route got
// none, which is why its crops went un-measured for a milestone while the VS
// ones were fitted to a histogram. This is the roster's equivalent. Run it:
//
//	make probe-roster
//	make probe-roster PROBE_ARGS='-roster.detail'         # per-band reads
//	make probe-roster PROBE_ARGS='-roster.x0sweep'        # sweep nameXFrac0
//	make probe-roster PROBE_ARGS='-roster.inkprofile'     # the column histogram
//
// It asserts nothing and always passes. Reading its output is the point.
//
// # What it can and cannot tell you
//
// There is no hand-checked transcription of a roster capture the way
// fixtures/m4gate/expected.yaml is one for the VS ranking, so this probe
// cannot report an accuracy. It scores against the 86 hand-transcribed names
// in the VS fixture, and that set is neither complete (the ranking lists
// scorers, and this alliance had 97 members) nor contemporaneous (three days
// separate the two captures). A correct read can therefore match nothing.
//
// So the headline "exact" count is a LOWER bound and must never be quoted as
// an accuracy. The number that carries the actual signal is `junk-prefixed`:
// reads that match a known name only after one leading token is stripped.
// That counter does not depend on the truth set being complete — every one of
// its hits is a read that is provably correct except for something the crop
// let in — and it is the direct measure of the defect the ink profile
// localized. A crop change is read against that column first.
//
// # Why an ink profile mode lives in here
//
// CLAUDE.md's rule is that a crop "verified by eye" is not measured, and the
// roster's own field fractions carry a comment recording that they were
// checked by reading ten cropped rows back by eye before OCR ran. They were
// wrong: nameXFrac0 = 0.19 put the crop's left edge inside the per-member
// status icon that sits between the avatar and the name, so every member who
// has one got a fragment of it glued to the front of their name. -roster.
// inkprofile is what that claim is made of, and it is in here rather than in
// a throwaway script so the next person moving this crop measures instead of
// reasoning.
package ingest

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/vision"
)

// rosterNameRetry mirrors vsNameRetry: a raw-line read of a crop that PSM 7's
// layout analysis refused to look at. The roster name field does NOT ship this
// -- processRow calls readField, not readFieldWithRetry -- and -roster.retry
// is how the case for changing that is made in numbers rather than by
// analogy. See CLAUDE.md, "Tesseract's layout analysis is blind to some
// perfectly legible crops", and note its warning that the retry's own
// preprocessing is a separate measurement from the primary's.
var rosterNameRetry = readPlan{
	spec: ocr.Spec{MinConf: nameSpec.MinConf, PSM: ocr.PSMRawLine},
	opts: vision.Options{SkipEqualize: true, SkipThreshold: true, UpscaleFactor: 4},
}

var (
	rosterDetail = flag.Bool("roster.detail", false,
		"print a per-band line: the read, and whether it matched, matched only after stripping, or missed")
	rosterX0Sweep = flag.Bool("roster.x0sweep", false,
		"sweep nameXFrac0 across the gutter and report each setting's counts")
	rosterInkProfile = flag.Bool("roster.inkprofile", false,
		"print the column ink histogram over the name line, which is how the crop's edges are placed")
	rosterFrames = flag.String("roster.frames", "",
		"path to the frame list (default fixtures/m4roster/frames.yaml)")
	rosterMaxFrames = flag.Int("roster.maxframes", 0,
		"read only the first N member-list frames (0 = all); the ink profile and sweeps are slow")
	rosterRetry = flag.Bool("roster.retry", false,
		"retry an empty read at PSM 13, the way the VS name field does; the roster name field does not, and this measures what that costs")
)

// leadingToken matches one short token followed by whitespace at the start of
// a read — the shape a clipped status icon leaves behind ("7 Kun Tsunami",
// "} Lothar232", "P Ravenna Morrigan"). Three characters rather than one
// because the icon can survive as a small cluster ('>}'), and anchored so it
// can never eat into the middle of a name.
var leadingToken = regexp.MustCompile(`^(\S{1,3})\s+(.+)$`)

type rosterProbeFrame struct {
	Seq      int    `yaml:"seq"`
	SHA256   string `yaml:"sha256"`
	OffsetPx int    `yaml:"offset_px"`
	GroupKey string `yaml:"group_key"`
}

type rosterProbeCapture struct {
	Capture int                `yaml:"capture"`
	Frames  []rosterProbeFrame `yaml:"frames"`
}

type rosterLoadedFrame struct {
	Seq int
	Img image.Image
}

// rosterBandRead is one row band's name read, kept so the sweep and the detail
// view can be built from a single pass over the pixels.
type rosterBandRead struct {
	Seq  int
	Y0   int
	Text string
}

type rosterCounts struct {
	Label string
	Bands int
	// Exact reads that equal a hand-transcribed name outright.
	Exact int
	// JunkPrefixed reads that equal one only after a leading token is
	// stripped. This is the defect counter; see the package comment.
	JunkPrefixed int
	Empty        int
	// Unmatched reads that matched nothing either way. Ambiguous by
	// construction: an unreadable name and a member absent from the truth
	// set are indistinguishable here, which is why this is not an error rate.
	Unmatched int
}

func (c rosterCounts) String() string {
	return fmt.Sprintf("%-22s bands %3d  exact %3d  junk-prefixed %3d  empty %3d  unmatched %3d",
		c.Label, c.Bands, c.Exact, c.JunkPrefixed, c.Empty, c.Unmatched)
}

func TestRosterNameProbe(t *testing.T) {
	ctx := context.Background()

	truth := loadRosterTruth(t)
	frames := loadRosterFrames(t, ctx)
	t.Logf("roster probe: %d member-list frames, %d hand-transcribed names to score against",
		len(frames), len(truth))
	t.Logf("  the truth set is the VS ranking's scorers, three days later: incomplete and not")
	t.Logf("  contemporaneous. `exact` is a lower bound, `junk-prefixed` is the defect measure.")

	if *rosterInkProfile {
		reportRosterInkProfile(t, frames)
		return
	}

	engine := ocr.NewTesseractEngine()

	x0s := []float64{nameXFrac0}
	if *rosterX0Sweep {
		// Spanning the avatar gutter, the status icon and the name's own left
		// edge, so the shape of the curve is visible rather than two points.
		x0s = []float64{0.190, 0.200, 0.210, 0.2153, 0.2167, 0.218, 0.2222, 0.230}
	}

	for _, x0 := range x0s {
		reads := readRosterNames(ctx, t, engine, frames, x0)
		counts := scoreRosterReads(reads, truth, fmt.Sprintf("nameXFrac0=%.4f", x0))
		t.Log(counts.String())

		if *rosterDetail {
			reportRosterDetail(t, reads, truth)
		}
	}
}

// readRosterNames runs the production read path over every row band of every
// frame at the given left edge. No dedupe: a member visible in three
// overlapping frames is scored three times, so one bad band reads differently
// from a systematic miss — the same choice zz_name_probe_test.go makes and for
// the same reason.
func readRosterNames(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame, x0 float64) []rosterBandRead {
	t.Helper()

	ing := &Ingester{engine: engine}
	var out []rosterBandRead
	for _, f := range frames {
		bands, err := SegmentRows(f.Img, memberListRegion, memberRowPitch)
		if err != nil {
			// A frame whose rows will not segment is a real observation about
			// the capture, not a reason to abandon the measurement.
			t.Logf("  frame %d: segmenting: %v", f.Seq, err)
			continue
		}
		for _, band := range bands {
			rect := fieldRect(band, f.Img, x0, nameXFrac1, topRowYFrac0, topRowYFrac1)
			var res ocr.Result
			var err error
			if *rosterRetry {
				res, _, err = ing.readFieldWithRetry(ctx, f.Img, rect,
					readPlan{spec: nameSpec, opts: nameOptions}, rosterNameRetry)
			} else {
				res, err = ing.readField(ctx, f.Img, rect, nameSpec, nameOptions)
			}
			if err != nil {
				t.Fatalf("reading frame %d band %d: %v", f.Seq, band.Y0, err)
			}
			out = append(out, rosterBandRead{Seq: f.Seq, Y0: band.Y0, Text: strings.TrimSpace(res.Text)})
		}
	}
	return out
}

func scoreRosterReads(reads []rosterBandRead, truth map[string]bool, label string) rosterCounts {
	c := rosterCounts{Label: label, Bands: len(reads)}
	for _, r := range reads {
		switch {
		case r.Text == "":
			c.Empty++
		case truth[r.Text]:
			c.Exact++
		case stripLeadingToken(r.Text) != r.Text && truth[stripLeadingToken(r.Text)]:
			c.JunkPrefixed++
		default:
			c.Unmatched++
		}
	}
	return c
}

func stripLeadingToken(s string) string {
	if m := leadingToken.FindStringSubmatch(s); m != nil {
		return m[2]
	}
	return s
}

func reportRosterDetail(t *testing.T, reads []rosterBandRead, truth map[string]bool) {
	t.Helper()
	t.Log("  per band:")
	for _, r := range reads {
		verdict := "unmatched"
		switch {
		case r.Text == "":
			verdict = "EMPTY"
		case truth[r.Text]:
			verdict = "exact"
		case stripLeadingToken(r.Text) != r.Text && truth[stripLeadingToken(r.Text)]:
			verdict = fmt.Sprintf("JUNK-PREFIXED -> %q", stripLeadingToken(r.Text))
		}
		t.Logf("    frame %2d y=%4d  read=%-28q %s", r.Seq, r.Y0, r.Text, verdict)
	}
}

// reportRosterInkProfile prints ink per column over the name line of every row
// band, which is how a column crop's edges are placed in this project.
//
// "Ink" is deviation from the scanline's own median colour rather than
// darkness. The member list is orange text on cream and white-with-outline
// text on the same cream, so a dark-pixel count — the measure the VS crops
// were placed with, against a dark ranking screen — reads near zero here and
// would put every edge in the wrong place.
func reportRosterInkProfile(t *testing.T, frames []rosterLoadedFrame) {
	t.Helper()

	const inkThreshold = 90 // summed |ΔR|+|ΔG|+|ΔB| against the scanline median

	var acc []int
	bandCount := 0
	for _, f := range frames {
		b := f.Img.Bounds()
		if acc == nil {
			acc = make([]int, b.Dx())
		}
		bands, err := SegmentRows(f.Img, memberListRegion, memberRowPitch)
		if err != nil {
			continue
		}
		for _, band := range bands {
			bandCount++
			y0 := band.Y0 + int(topRowYFrac0*float64(band.Height()))
			y1 := band.Y0 + int(topRowYFrac1*float64(band.Height()))
			for y := y0; y < y1 && y < b.Max.Y; y++ {
				med := scanlineMedian(f.Img, y)
				for x := b.Min.X; x < b.Max.X; x++ {
					if colourDistance(f.Img, x, y, med) > inkThreshold {
						acc[x]++
					}
				}
			}
		}
	}

	max := 0
	for _, v := range acc {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		t.Log("  ink profile: no ink found; the frames or the region are wrong")
		return
	}
	t.Logf("  ink profile over %d row bands, name line only. x, fraction, ink:", bandCount)
	for x := range acc {
		frac := float64(x) / float64(len(acc))
		if frac < 0.10 || frac > 0.45 {
			continue
		}
		bar := strings.Repeat("#", acc[x]*40/max)
		note := ""
		if x == int(nameXFrac0*float64(len(acc))) {
			note = "  <- nameXFrac0"
		}
		t.Logf("    x=%3d %.4f %-40s %d%s", x, frac, bar, acc[x], note)
	}
}

func scanlineMedian(img image.Image, y int) [3]uint32 {
	b := img.Bounds()
	var rs, gs, bs []uint32
	for x := b.Min.X; x < b.Max.X; x++ {
		r, g, bl, _ := img.At(x, y).RGBA()
		rs = append(rs, r>>8)
		gs = append(gs, g>>8)
		bs = append(bs, bl>>8)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
	sort.Slice(gs, func(i, j int) bool { return gs[i] < gs[j] })
	sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
	m := len(rs) / 2
	return [3]uint32{rs[m], gs[m], bs[m]}
}

func colourDistance(img image.Image, x, y int, med [3]uint32) int {
	r, g, b, _ := img.At(x, y).RGBA()
	d := absDiff(r>>8, med[0]) + absDiff(g>>8, med[1]) + absDiff(b>>8, med[2])
	return int(d)
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// loadRosterTruth reads the hand-transcribed names out of the VS gate's
// fixture. It is the only hand-checked name set this project has; see the
// package comment for what that borrowing costs.
func loadRosterTruth(t *testing.T) map[string]bool {
	t.Helper()

	path := filepath.Join("..", "..", "fixtures", "m4gate", "expected.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no hand-checked names at %s", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var exp struct {
		Rows []struct {
			Name string `yaml:"name"`
		} `yaml:"rows"`
	}
	if err := yaml.Unmarshal(data, &exp); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	out := make(map[string]bool, len(exp.Rows))
	for _, r := range exp.Rows {
		out[r.Name] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s carries no names", path)
	}
	return out
}

func loadRosterFrames(t *testing.T, ctx context.Context) []rosterLoadedFrame {
	t.Helper()

	path := *rosterFrames
	if path == "" {
		path = filepath.Join("..", "..", "fixtures", "m4roster", "frames.yaml")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no roster frame list at %s", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cap rosterProbeCapture
	if err := yaml.Unmarshal(data, &cap); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load(): %v", err)
	}
	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		t.Fatalf("opening blob store (%s): %v", cfg.Blob.Backend, err)
	}

	sort.Slice(cap.Frames, func(i, j int) bool { return cap.Frames[i].Seq < cap.Frames[j].Seq })
	var out []rosterLoadedFrame
	for _, f := range cap.Frames {
		// The alliance-summary frame carries the "Members: 96/100" line and no
		// member rows at all.
		if f.GroupKey != "" {
			continue
		}
		if *rosterMaxFrames > 0 && len(out) >= *rosterMaxFrames {
			break
		}
		rc, err := blobs.Get(ctx, blob.Key(f.SHA256))
		if err != nil {
			t.Skipf("frame %d (%s) is not in the %s blob store; set LW_BLOB_FS_ROOT to an absolute path (see CLAUDE.md)",
				f.Seq, f.SHA256, cfg.Blob.Backend)
		}
		img, err := png.Decode(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("decoding frame %d: %v", f.Seq, err)
		}
		out = append(out, rosterLoadedFrame{Seq: f.Seq, Img: img})
	}
	if len(out) == 0 {
		t.Fatalf("%s lists no member-list frames", path)
	}
	return out
}
