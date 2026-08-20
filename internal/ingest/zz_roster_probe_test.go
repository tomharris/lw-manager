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
// It grew four more instruments once the roster gate started failing, because
// the name column was the only field on this route that had one and the name
// column turned out not to be where the damage was:
//
//	make probe-roster PROBE_ARGS='-roster.badge'          # the rank NCC read
//	make probe-roster PROBE_ARGS='-roster.header'         # the group header OCR
//	make probe-roster PROBE_ARGS='-roster.headerink'      # the header histogram
//	make probe-roster PROBE_ARGS='-roster.power'          # the power column
//	make probe-roster PROBE_ARGS='-roster.level'          # the level column
//
// Only -roster.badge measures something that is not an OCR read: rank comes
// from hand-rolled NCC against embedded badge crops (rankbadge.go), never from
// OCR of the badge's outlined glyphs, so no amount of work on the OCR modes
// could ever have seen a rank defect.
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
	rosterBadge = flag.Bool("roster.badge", false,
		"report matchRankBadge's per-frame verdict: winning rank, runner-up, gap, against the fixture's own group")
	rosterHeader = flag.Bool("roster.header", false,
		"report what each frame's group header reads, its parsed N/M, and the transcribed truth beside it")
	rosterHeaderInk = flag.Bool("roster.headerink", false,
		"column ink histogram over groupHeaderRegion — where a crop edge should go")
	rosterPower = flag.Bool("roster.power", false,
		"report what the power field reads per band, and whether ParsePower accepts it")
	rosterLevel = flag.Bool("roster.level", false,
		"report what the level field reads per band, and whether ParseLevel accepts it")
	rosterBadgeShuffle = flag.Bool("roster.badgeshuffle", false,
		"rotate every truth label one rank forward, so the badge mode's verdicts are wrong by construction; it must report ~0 agree")
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

	// Each mode below returns rather than falling through to the name pass.
	// They are independent instruments and the default pass costs an OCR call
	// per row band on every frame; running it as a side effect of asking about
	// the power column would make the cheap questions expensive.
	if *rosterInkProfile {
		reportRosterInkProfile(t, frames)
		return
	}
	if *rosterHeaderInk {
		reportRosterHeaderInkProfile(t, frames)
		return
	}
	if *rosterBadge {
		truthRanks := rosterFrameRanks()
		if *rosterBadgeShuffle {
			// The validation pass, modelled on -probe.assignshuffle. See
			// reportRosterBadge's doc comment for why a 61/61 result is not
			// believable until this has been run.
			truthRanks = rotateRosterFrameRanks(truthRanks)
			t.Log("  -roster.badgeshuffle: every truth label rotated one rank forward.")
			t.Log("  Every verdict below is wrong by construction. Anything but ~0 agree means")
			t.Log("  this mode cannot report a disagreement and its clean run proved nothing.")
		}
		reportRosterBadge(ctx, t, frames, truthRanks)
		return
	}

	engine := ocr.NewTesseractEngine()

	if *rosterHeader || *rosterPower || *rosterLevel {
		if *rosterHeader {
			reportRosterHeader(ctx, t, engine, frames)
		}
		if *rosterPower {
			reportRosterField(ctx, t, engine, frames, rosterPowerField)
		}
		if *rosterLevel {
			reportRosterField(ctx, t, engine, frames, rosterLevelField)
		}
		return
	}

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
	return loadRosterFramesFiltered(t, ctx, false)
}

// loadRosterFramesFiltered is loadRosterFrames with the alliance-summary frame
// selectable. Every mode but one wants the member-list frames; -roster.badge
// wants the summary frame on its own, as a negative control — it is the one
// frame in the capture that carries no rank badge at all, so what
// bestTwoRankScores makes of it is the only evidence in this file that the
// instrument can report the answer nobody wants (CLAUDE.md, "A clean
// measurement is not a validated one").
func loadRosterFramesFiltered(t *testing.T, ctx context.Context, wantSummary bool) []rosterLoadedFrame {
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
		if (f.GroupKey != "") != wantSummary {
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
		t.Fatalf("%s lists no matching frames", path)
	}
	return out
}

// ---------------------------------------------------------------------------
// The rank badge: the one decision on this route that is not an OCR read.
// ---------------------------------------------------------------------------

// rosterFrameRanks maps a frame's seq to the rank its sticky header actually
// shows, which is the rank every row on that frame belongs to.
//
// It is NOT recoverable from fixtures/m4rostergate/expected.yaml. That fixture
// records a rank per *member* and a total per *group*, and it copies the frame
// list verbatim from fixtures/m4roster/frames.yaml — which carries seq, sha256
// and offset_px and nothing about which group a frame is looking at. Nothing
// in either file joins a member to a frame, so there is no derivation to do;
// the brief anticipated this and asked for the mapping to be read off the
// frames and said to be read off the frames rather than guessed.
//
// So it was read off the frames, by eye, at 2x, from the header band of all 62
// of them — the group NAME ("This Is It" / "Footloose" / "I'm Alright"), which
// the fixture's `groups:` block maps one-to-one onto a rank, not the badge
// glyph this mode is measuring. Reading the name rather than the badge is what
// keeps the truth independent of the instrument: an eye-read of the badge
// would be scoring NCC's crop against a human's reading of the same 16x22
// pixels, which is not a second opinion.
//
// CLAUDE.md is blunt that an eye-check is not a measurement, and it is right
// twice over on this project's own history. What it caught both times was a
// human confirming something they already knew (a name they could read past a
// clipped icon); the failure mode is confirmation, not identification. Picking
// which of three distinct words is printed in 30px type is the second thing.
// It is nonetheless cross-checked mechanically by reportRosterBadge, which
// classifies each header band by its own background colour — R2's header is
// green and R3's and R4's are lavender — and reports every frame where the
// colour and this table disagree. That check cannot separate R3 from R4, and
// says so in the output; it can separate R2 from R3, which is the whole of the
// confusion the gate found.
//
// The distribution: seq 0 is the alliance summary and has no header at all,
// seq 1 is R4's collapsed header, and the remaining 60 alternate between R3
// and R2 as roster_capture sweeps the same stretch of list repeatedly (39 R3,
// 21 R2). R1 never appears as a sticky header in this capture.
func rosterFrameRanks() map[int]string {
	out := map[int]string{1: "R4"}
	for _, r := range []struct {
		rank     string
		from, to int
	}{
		{"R3", 2, 22}, {"R2", 23, 25}, {"R3", 26, 28}, {"R2", 29, 30},
		{"R3", 31, 33}, {"R2", 34, 36}, {"R3", 37, 38}, {"R2", 39, 41},
		{"R3", 42, 43}, {"R2", 44, 46}, {"R3", 47, 49}, {"R2", 50, 51},
		{"R3", 52, 53}, {"R2", 54, 56}, {"R3", 57, 59}, {"R2", 60, 61},
	} {
		for s := r.from; s <= r.to; s++ {
			out[s] = r.rank
		}
	}
	return out
}

// rotateRosterFrameRanks maps every truth label to the next rank in
// rankBadgeOrder, so no frame can be labelled with the rank it actually shows.
// It is the roster badge's -probe.assignshuffle: CLAUDE.md, "A clean
// measurement is not a validated one" -- the assignment probe reported a
// perfect result from an instrument that had never been shown capable of
// reporting a wrong one, and rotating the labels was what settled it.
func rotateRosterFrameRanks(truth map[int]string) map[int]string {
	next := map[string]string{}
	for i, r := range rankBadgeOrder {
		next[r] = rankBadgeOrder[(i+1)%len(rankBadgeOrder)]
	}
	out := make(map[int]string, len(truth))
	for seq, rank := range truth {
		out[seq] = next[rank]
	}
	return out
}

// reportRosterBadge reports what matchRankBadge decides on every frame, beside
// what that frame's header actually shows.
//
// The gate found the whole of capture 1 attributed to R3 -- R2's rows
// included -- and no probe in this file could have seen that, because every
// other mode measures OCR and this decision is NCC. rankBadgeMinGap (0.20) is
// the constant in question and it has a documented history: it was raised from
// 0.15 after a reviewer produced a MEASURED near-miss at gap 0.162 that was
// accepted at the wrong rank. Half a distribution is what set it the first
// time; this is the other half.
//
// It reports three things a bare pass count cannot:
//
//   - a want x got matrix, because "N wrong" does not say which rank is
//     absorbing the others;
//   - the gap distribution split by verdict, because a wrong verdict at a wide
//     gap says the templates match the wrong thing while a wrong verdict at a
//     narrow gap says the threshold cannot separate them, and those need
//     opposite fixes;
//   - what the shipped acceptance rule does with each frame, since
//     bestTwoRankScores applies none: a wrong argmax that rankBadgeMinGap
//     refuses costs a frame to review, and a wrong argmax it accepts writes
//     rows under the wrong group.
//
// Two things keep its own numbers honest, and both should be run before a
// headline out of this mode is believed. -roster.badgeshuffle rotates every
// truth label one rank forward so the whole run is wrong by construction (it
// must report 0 agree), and the negative control at the end scores the one
// frame in the capture that carries no badge at all. Without those, a clean
// sweep is indistinguishable from an instrument that returns the same answer
// whatever it is shown -- which is precisely the trap CLAUDE.md records the
// assignment probe walking into.
func reportRosterBadge(ctx context.Context, t *testing.T, frames []rosterLoadedFrame, truth map[int]string) {
	t.Helper()

	agree, disagree, refused := 0, 0, 0
	acceptedWrong, refusedByGap := 0, 0
	colourDisagree := 0
	matrix := map[string]map[string]int{}
	var agreeGaps, disagreeGaps []float64

	for _, f := range frames {
		want := truth[f.Seq]
		if want == "" {
			t.Logf("  seq %2d  NO TRUTH RECORDED; skipping", f.Seq)
			continue
		}

		// The independent cross-check on the truth table above. R2's header
		// band is green, R3's and R4's are lavender, so this separates R2 from
		// non-R2 and nothing finer -- which is exactly the split the gate's
		// defect lives on.
		green := rosterHeaderIsGreen(f.Img)
		if green != (want == "R2") {
			colourDisagree++
			t.Logf("  seq %2d  TRUTH CROSS-CHECK FAILED: table says %s, header background says %s",
				f.Seq, want, map[bool]string{true: "R2 (green)", false: "not R2 (lavender)"}[green])
		}

		best, runnerUp, err := bestTwoRankScores(f.Img)
		if err != nil {
			refused++
			t.Logf("  seq %2d  want %-3s  REFUSED: %v", f.Seq, want, err)
			continue
		}
		gap := best.score - runnerUp.score
		accepted := gap >= rankBadgeMinGap
		if !accepted {
			refusedByGap++
		}
		if matrix[want] == nil {
			matrix[want] = map[string]int{}
		}
		matrix[want][best.rank]++

		verdict := "ok      "
		if best.rank != want {
			verdict = "<-- WRONG"
			disagree++
			disagreeGaps = append(disagreeGaps, gap)
			if accepted {
				acceptedWrong++
			}
		} else {
			agree++
			agreeGaps = append(agreeGaps, gap)
		}
		t.Logf("  seq %2d  want %-3s  got %-3s %s  best %.3f  runner-up %s %.3f  gap %.3f  %s",
			f.Seq, want, best.rank, verdict, best.score, runnerUp.rank, runnerUp.score, gap,
			map[bool]string{true: "accepted", false: "REFUSED by rankBadgeMinGap"}[accepted])
	}

	t.Logf("  badge: %d agree, %d WRONG, %d refused by bestTwoRankScores, of %d frames",
		agree, disagree, refused, len(frames))
	t.Logf("  of those, %d were refused by rankBadgeMinGap=%.2f and %d WRONG verdicts were ACCEPTED",
		refusedByGap, rankBadgeMinGap, acceptedWrong)
	t.Logf("  truth cross-check (header background colour vs the seq->rank table): %d disagreements",
		colourDisagree)

	t.Log("  want x got:")
	for _, want := range rankBadgeOrder {
		row, ok := matrix[want]
		if !ok {
			continue
		}
		var parts []string
		for _, got := range rankBadgeOrder {
			if row[got] > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", got, row[got]))
			}
		}
		t.Logf("    want %-3s -> %s", want, strings.Join(parts, "  "))
	}

	reportGapDistribution(t, "agreeing", agreeGaps)
	reportGapDistribution(t, "WRONG   ", disagreeGaps)

	// The negative control. CLAUDE.md: "before believing a measurement,
	// establish that it can produce the answer you are hoping not to see."
	// Every frame above carries a real badge, so a clean sweep would be
	// indistinguishable from an instrument that returns the same rank
	// regardless. The alliance-summary frame carries no badge at all.
	control := loadRosterFramesFiltered(t, ctx, true)
	for _, f := range control {
		best, runnerUp, err := bestTwoRankScores(f.Img)
		if err != nil {
			t.Logf("  control (no badge on frame, seq %d): REFUSED: %v", f.Seq, err)
			continue
		}
		gap := best.score - runnerUp.score
		t.Logf("  control (no badge on frame, seq %d): best %s %.3f  runner-up %s %.3f  gap %.3f  -> %s",
			f.Seq, best.rank, best.score, runnerUp.rank, runnerUp.score, gap,
			map[bool]string{true: "ACCEPTED (the gap check does not defend this frame)", false: "refused, as it should be"}[gap >= rankBadgeMinGap])
	}
}

// reportGapDistribution buckets gaps so a headline count cannot hide the shape
// of what produced it. rankBadgeMinGap sits at 0.20, so the buckets are placed
// around it rather than evenly.
func reportGapDistribution(t *testing.T, label string, gaps []float64) {
	t.Helper()
	if len(gaps) == 0 {
		t.Logf("  gap distribution, %s: none", label)
		return
	}
	edges := []float64{0.00, 0.05, 0.10, 0.15, 0.20, 0.30, 0.40, 0.60, 1.01}
	counts := make([]int, len(edges)-1)
	min, max, sum := gaps[0], gaps[0], 0.0
	for _, g := range gaps {
		sum += g
		if g < min {
			min = g
		}
		if g > max {
			max = g
		}
		for i := 0; i < len(counts); i++ {
			if g >= edges[i] && g < edges[i+1] {
				counts[i]++
				break
			}
		}
	}
	var parts []string
	for i, c := range counts {
		if c > 0 {
			parts = append(parts, fmt.Sprintf("[%.2f,%.2f)=%d", edges[i], edges[i+1], c))
		}
	}
	t.Logf("  gap distribution, %s (n=%d, min %.3f, mean %.3f, max %.3f): %s",
		label, len(gaps), min, sum/float64(len(gaps)), max, strings.Join(parts, "  "))
}

// rosterHeaderIsGreen classifies the sticky header band by its own background
// colour, which is the independent signal rosterFrameRanks is cross-checked
// against. It samples to the right of the badge and to the left of the
// chevron, so neither the thing being measured nor the noisiest part of the
// crop is in it.
func rosterHeaderIsGreen(img image.Image) bool {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	x0, x1 := b.Min.X+int(0.55*float64(w)), b.Min.X+int(0.80*float64(w))
	y0, y1 := b.Min.Y+int(groupHeaderRegion.Y1*float64(h)), b.Min.Y+int(groupHeaderRegion.Y2*float64(h))
	var rs, gs, bs []int
	for y := y0; y < y1 && y < b.Max.Y; y++ {
		for x := x0; x < x1 && x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			rs = append(rs, int(r>>8))
			gs = append(gs, int(g>>8))
			bs = append(bs, int(bl>>8))
		}
	}
	if len(rs) == 0 {
		return false
	}
	sort.Ints(rs)
	sort.Ints(gs)
	sort.Ints(bs)
	m := len(rs) / 2
	// Lavender has blue above green; green has green above both.
	return gs[m] > rs[m]+6 && gs[m] > bs[m]+6
}

// ---------------------------------------------------------------------------
// The group header: the field whose refusal drops a whole group.
// ---------------------------------------------------------------------------

// reportRosterHeader reads groupHeaderRegion on every frame and reports the
// raw text, what parseGroupHeader made of it, and why it refused.
//
// A header that will not parse makes IngestRoster `continue` before
// SegmentRows is ever called, so the whole frame is dropped -- and the review
// queue's raw text names the cause: "R2) I'm Alright VN iy]" keeps the group
// name and loses the N/M count, and the chevron sits inside
// groupHeaderRegion's right edge at X2=0.97.
//
// It groups identical reads at the end rather than only listing them, because
// this file's own warning applies here more than anywhere: a header mode that
// reported one string for every frame would be measuring a constant, not a
// crop. The frames show three different groups, so the distinct-read count is
// the tell (CLAUDE.md, "A broken instrument reports agreement, not noise").
func reportRosterHeader(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame) {
	t.Helper()
	ing := New(nil, nil, engine)
	truth := rosterFrameRanks()

	ok, bad := 0, 0
	lowConf := 0
	distinct := map[string]int{}
	byReason := map[string]int{}
	for _, f := range frames {
		res, err := ing.readField(ctx, f.Img, groupHeaderRegion, groupHeaderSpec, groupHeaderOptions)
		if err != nil {
			t.Logf("  seq %2d  want %-3s  READ ERROR %v", f.Seq, truth[f.Seq], err)
			bad++
			continue
		}
		distinct[res.Text]++
		if !res.Accepted(groupHeaderSpec) {
			lowConf++
		}
		name, total, perr := parseGroupHeader(res.Text)
		if perr != nil {
			bad++
			byReason[rosterHeaderRefusalReason(perr)]++
			t.Logf("  seq %2d  want %-3s  conf %.2f  %-44q  REFUSED: %v", f.Seq, truth[f.Seq], res.Confidence, res.Text, perr)
			continue
		}
		ok++
		t.Logf("  seq %2d  want %-3s  conf %.2f  %-44q  -> %q total=%d", f.Seq, truth[f.Seq], res.Confidence, res.Text, name, total)
	}
	t.Logf("  header: %d parsed, %d refused, of %d frames (%d read below groupHeaderSpec.MinConf=%.2f)",
		ok, bad, len(frames), lowConf, groupHeaderSpec.MinConf)
	t.Logf("  distinct raw reads: %d over %d frames -- one or two would mean this mode is measuring a constant, not a crop",
		len(distinct), len(frames))
	for reason, n := range byReason {
		t.Logf("    refusal shape %-28s %d", reason, n)
	}
}

// rosterHeaderRefusalReason collapses parseGroupHeader's error text to the
// shape of the refusal, so the summary can say which of its two guards fired
// rather than only how often one of them did.
func rosterHeaderRefusalReason(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "found 0 count-shaped tokens"):
		return "no N/M token"
	case strings.Contains(s, "count-shaped tokens"):
		return "several N/M tokens"
	case strings.Contains(s, "not a coherent N/M"):
		return "incoherent N/M"
	default:
		return "other"
	}
}

// reportRosterHeaderInkProfile is reportRosterInkProfile aimed at the sticky
// header band instead of the row bands, and printed at full width rather than
// through the name field's 0.10-0.45 window: the edge under suspicion is
// X2=0.97, which that window would hide entirely.
func reportRosterHeaderInkProfile(t *testing.T, frames []rosterLoadedFrame) {
	t.Helper()

	const inkThreshold = 90 // summed |ΔR|+|ΔG|+|ΔB| against the scanline median

	var acc []int
	rows := 0
	for _, f := range frames {
		b := f.Img.Bounds()
		if acc == nil {
			acc = make([]int, b.Dx())
		}
		y0 := b.Min.Y + int(groupHeaderRegion.Y1*float64(b.Dy()))
		y1 := b.Min.Y + int(groupHeaderRegion.Y2*float64(b.Dy()))
		for y := y0; y < y1 && y < b.Max.Y; y++ {
			rows++
			med := scanlineMedian(f.Img, y)
			for x := b.Min.X; x < b.Max.X; x++ {
				if colourDistance(f.Img, x, y, med) > inkThreshold {
					acc[x]++
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
		t.Log("  header ink profile: no ink found; the frames or the region are wrong")
		return
	}
	t.Logf("  header ink profile over %d frames, %d scanlines, full frame width. x, fraction, ink:",
		len(frames), rows)
	for x := range acc {
		note := ""
		switch x {
		case int(groupHeaderRegion.X1 * float64(len(acc))):
			note = "  <- groupHeaderRegion.X1"
		case int(groupHeaderRegion.X2 * float64(len(acc))):
			note = "  <- groupHeaderRegion.X2"
		case int(rankBadgeRegion.X1 * float64(len(acc))):
			note = "  <- rankBadgeRegion.X1"
		case int(rankBadgeRegion.X2 * float64(len(acc))):
			note = "  <- rankBadgeRegion.X2"
		}
		bar := strings.Repeat("#", acc[x]*40/max)
		t.Logf("    x=%3d %.4f %-40s %d%s", x, float64(x)/float64(len(acc)), bar, acc[x], note)
	}
}

// ---------------------------------------------------------------------------
// Power and level: two fields that have produced zero facts and no instrument.
// ---------------------------------------------------------------------------

// rosterFieldProbe is one member-row field's production read path, named so a
// single report function can measure either without a switch inside its loop.
type rosterFieldProbe struct {
	Name           string
	XFrac0, XFrac1 float64
	YFrac0, YFrac1 float64
	Spec           ocr.Spec
	Opts           vision.Options
	// Parse is the production parser. It returns a printable value so the
	// probe reports what would have been written, not just that something was.
	Parse func(string) (string, error)
	// Shapes classify a refusal. A count of refusals says how much is broken;
	// a shape says what KIND of broken, which is the difference between a
	// number a crop change should move and a number it cannot touch. First
	// match wins, so order them most specific first.
	Shapes []rosterRefusalShape
}

type rosterRefusalShape struct {
	Name string
	Re   *regexp.Regexp
}

var rosterPowerField = rosterFieldProbe{
	Name:   "power",
	XFrac0: powerXFrac0, XFrac1: powerXFrac1,
	YFrac0: bottomRowYFrac0, YFrac1: bottomRowYFrac1,
	Spec: powerSpec, Opts: powerOptions,
	Parse: func(s string) (string, error) {
		v, err := ParsePower(s)
		return fmt.Sprintf("%d", v), err
	},
	Shapes: []rosterRefusalShape{{"ONE DAMAGED SEPARATOR", powerOneBadSeparator}},
}

var rosterLevelField = rosterFieldProbe{
	Name:   "level",
	XFrac0: levelXFrac0, XFrac1: levelXFrac1,
	YFrac0: bottomRowYFrac0, YFrac1: bottomRowYFrac1,
	Spec: levelSpec, Opts: levelOptions,
	Parse: func(s string) (string, error) {
		v, err := ParseLevel(s)
		return fmt.Sprintf("%d", v), err
	},
	Shapes: []rosterRefusalShape{{"LOST LEADING L", levelLostLeadingL}},
}

// levelLostLeadingL is the level column's counterpart to
// powerOneBadSeparator: a read that is the level with the "L" of "Lv." gone,
// so ParseLevel's prefix check refuses a string whose digits are right there.
// The same argument applies -- it is the shape a crop change should move, and
// counting it separately keeps it from being averaged into refusals no crop
// can help.
var levelLostLeadingL = regexp.MustCompile(`^[vV]\.?\s*\d`)

// powerOneBadSeparator matches a power read that is structurally correct
// except for the separator between the label and the number: "Power:}175'1M"
// against a frame that renders "Power: 211.5M". That shape is what the review
// queue is full of, and it is the number a crop change should move -- a
// refusal of this shape says the digits were read and the punctuation was not,
// which is a different problem from a refusal with no digits in it at all.
//
// It is deliberately loose about what sits between "Power" and the digits (up
// to two characters of anything, then optional space) and about the decimal
// point itself, because the whole point is that those are the characters
// coming back damaged.
var powerOneBadSeparator = regexp.MustCompile(`^Power.{0,2}\s*\d+.\d+M`)

// reportRosterField walks every row band of every frame exactly as
// readRosterNames does, reads one field through the production path, and
// reports what the parser made of it.
//
// Neither field has ever had an instrument, and between them they account for
// every fact the roster route fails to write: the gate's first run produced
// zero power facts and zero level facts from a capture holding 75 members.
// There is no hand-checked value to score against here the way there is for
// names, so this reports parse outcomes and shapes rather than an accuracy --
// what a value SAYS is a question for the gate's own expected.yaml, and this
// probe would be overclaiming to answer it.
func reportRosterField(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame, fp rosterFieldProbe) {
	t.Helper()
	ing := New(nil, nil, engine)

	bands, accepted, refused, empty, lowConf := 0, 0, 0, 0, 0
	distinct := map[string]int{}
	byShape := map[string]int{}
	for _, f := range frames {
		rows, err := SegmentRows(f.Img, memberListRegion, memberRowPitch)
		if err != nil {
			t.Logf("  frame %d: segmenting: %v", f.Seq, err)
			continue
		}
		for _, band := range rows {
			bands++
			rect := fieldRect(band, f.Img, fp.XFrac0, fp.XFrac1, fp.YFrac0, fp.YFrac1)
			res, err := ing.readField(ctx, f.Img, rect, fp.Spec, fp.Opts)
			if err != nil {
				t.Fatalf("reading frame %d band %d: %v", f.Seq, band.Y0, err)
			}
			text := strings.TrimSpace(res.Text)
			distinct[text]++
			if !res.Accepted(fp.Spec) {
				lowConf++
			}
			if text == "" {
				empty++
				t.Logf("    frame %2d y=%4d  conf %.2f  read=%-26q EMPTY", f.Seq, band.Y0, res.Confidence, text)
				continue
			}
			val, perr := fp.Parse(text)
			if perr != nil {
				refused++
				shape := "other"
				for _, sh := range fp.Shapes {
					if sh.Re.MatchString(text) {
						shape = sh.Name
						break
					}
				}
				byShape[shape]++
				t.Logf("    frame %2d y=%4d  conf %.2f  read=%-26q REFUSED (%s)", f.Seq, band.Y0, res.Confidence, text, shape)
				continue
			}
			accepted++
			t.Logf("    frame %2d y=%4d  conf %.2f  read=%-26q -> %s", f.Seq, band.Y0, res.Confidence, text, val)
		}
	}

	t.Logf("  %s: bands %d, parser accepted %d, parser refused %d, empty %d",
		fp.Name, bands, accepted, refused, empty)
	t.Logf("  %s: %d reads scored below %sSpec.MinConf=%.2f (factConfidenceGate is %.2f, and a fact needs both)",
		fp.Name, lowConf, fp.Name, fp.Spec.MinConf, factConfidenceGate)
	for _, sh := range fp.Shapes {
		t.Logf("  %s: %d of the %d refusals are structurally %s (%s) -- the digits were read and",
			fp.Name, byShape[sh.Name], refused, sh.Name, sh.Re)
		t.Logf("         one character around them was not. That is the count a crop change should")
		t.Logf("         move; the %d refusals shaped \"other\" are a different problem.", byShape["other"])
	}
	t.Logf("  %s: %d distinct reads over %d bands -- near-uniformity here would mean this mode is measuring a constant",
		fp.Name, len(distinct), bands)
}
