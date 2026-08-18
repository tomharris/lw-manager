//go:build m4probe

// The M4 points probe: how many of the gate's 86 rows does the *points* field
// actually read, and which ones does it get wrong?
//
// It is the sibling of zz_name_probe_test.go and exists for the reason that
// probe's header gives — the numbers that set vsPointsOptions came from a
// harness nobody committed — plus one this project learned later. The gate
// reports a single count of rows written, and that count moves for two
// unrelated reasons: the name did not resolve, or the name resolved and the
// points did not. Cross-referencing the gate's 21 failures against the name
// probe's 13 showed 8 rows whose name reads perfectly well, and nothing in the
// tree could say anything about them. Two aggregates side by side are not a
// causal claim; this is the second per-item view.
//
//	make probe-points                                    # the chosen setting
//	make probe-points PROBE_ARGS='-points.detail'        # per-row, to localize
//	make probe-points PROBE_ARGS='-points.sweep'         # the full options sweep
//	make probe-points PROBE_ARGS='-points.charset=0123456789,'   # re-measure the no-charset decision
//	make probe-points PROBE_ARGS='-points.fbsweep'       # sweep the RETRY's preprocessing, primary fixed
//
// It is a measuring instrument, not a gate: it asserts nothing and always
// passes. Reading its output is the point.
//
// The headline number is deliberately roster-free. The name probe scores reads
// against the known names; this one scores parsed values against the known
// points, with no name matching involved. That independence is the whole
// design: attributing a band to a row through the name field would make every
// points number conditional on the name field's 73/86, and the 13 members
// whose names cannot be read would become invisible exactly when the question
// is whether their points read fine. Eight-digit values make a coincidental
// match against the wrong row's total negligible, so the set-membership test
// stands on its own.
//
// Name attribution appears only under -points.detail, where the question
// changes from "does this field work" to "which row is broken" and a name is
// the only handle a human has on that. It is also the only way an empty or
// unparseable read can be filed against a row at all -- such a read carries no
// value to match on -- so the default run counts those in the aggregate and
// the detail run names them.
//
// Like the name probe it reads no database and does not apply IngestVS's
// geometric dedupe: every band of every frame is scored, so a row visible in
// three overlapping frames gets three chances and one bad band is visibly
// different from a systematic miss.
package ingest

import (
	"context"
	"flag"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
	"github.com/tomharris/lw-manager/internal/vision"
)

// Flags carry a points. prefix because this file and the name probe share a
// build tag and a package, so they register into the same flag set and a
// duplicate name panics at init rather than failing a test.
var (
	pointsDetail = flag.Bool("points.detail", false,
		"print a per-row line for every row that did not read cleanly: expected, raw read, parsed value, confidence, verdict")
	pointsSweep = flag.Bool("points.sweep", false,
		"sweep all eight preprocess shapes at upscale 2/3/4 instead of the shipped options")
	pointsPSMs = flag.String("points.psm", "",
		"comma-separated page-segmentation modes to compare, e.g. '7,13'; empty uses the engine default")
	pointsCharset = flag.String("points.charset", "",
		"set vsPointsSpec.Charset, e.g. '0123456789,'; empty ships as-is. The charset was removed deliberately -- see vsPointsSpec's comment -- and this re-measures that decision rather than re-reasoning it")
	pointsDump = flag.String("points.dump", "",
		"directory to write the crops of bands that did not read exactly: both the raw band and the preprocessed points crop, so the pixels can be looked at rather than reasoned about")
	pointsFBSweep = flag.Bool("points.fbsweep", false,
		"sweep the RETRY's preprocessing over the same grid while the primary stays fixed at the shipped setting -- vsPointsRetry inherited vsPointsOptions, and inheriting is an assumption, not a result; mirrors the name probe's -probe.fbsweep")
	pointsFallbackPSM = flag.Int("points.psmfallback", ocr.PSMRawLine,
		"page-segmentation mode the retry runs at when the primary read is empty; matches vsPointsRetry's shipped PSM 13 by default")
)

// pointsResult is one configuration's score.
//
// Both a band denominator and a row denominator are reported. A band is what
// the field is actually asked to read; a row may appear in three overlapping
// frames. A field that reads every row correctly in one frame out of three is a
// different problem from one that reads two thirds of the rows in every frame,
// and a single denominator cannot tell them apart.
type pointsResult struct {
	Label     string
	TotalBand int

	Exact       int // parsed value equals some expected row's points
	Within1Pct  int // ... or lands within the gate's own 1% tolerance
	Unparseable int // read did not clear pointsRe
	Empty       int // OCR returned nothing
	LowConf     int // parsed, but below MinConf: would queue rather than write

	// Best is each expected row's best showing across every band, keyed by
	// rank, so a row that read correctly once is never reported by one of its
	// worse bands.
	Best map[int]pointsRead

	// Joint is the set of ranks for which some *single* band yielded both an
	// auto-accepted name and an exact points read.
	//
	// This is the number the gate actually turns on, and it is not the smaller
	// of the two field accuracies. IngestVS reads both fields from one band and
	// writes a fact only when both succeed there, while each probe's headline
	// counts a row that succeeded in any band of any frame. A row whose name
	// resolves only in frame 3 and whose points read only in frame 7 counts in
	// both headlines and is written by neither. Populated under -points.detail,
	// which is the only mode that reads the name field.
	Joint map[int]bool

	// want is rank -> hand-checked points, carried so scoring can rank one
	// read against another without threading the fixture through every call.
	want map[int]int64
}

// pointsRead is one band's points read, kept with enough context to tell the
// failure modes apart without going back to the pixels: the raw text says
// whether the crop caught the right glyphs, Parsed says whether ParsePoints
// accepted them, and Conf says whether the pipeline would have trusted the
// result even so.
type pointsRead struct {
	Text   string
	Value  int64
	Parsed bool
	Conf   float64
	Frame  int
	Y0     int

	// Name is the member the band's name field resolved to, or "" if it did
	// not auto-accept. Populated only under -points.detail.
	Name string
}

func TestM4PointsProbe(t *testing.T) {
	ctx := context.Background()

	exp := loadProbeCapture(t)
	engine := ocr.NewTesseractEngine()
	if !engine.Available() {
		t.Skip("tesseract is not on PATH; see the gate's skip message")
	}

	frames := loadProbeFrames(t, ctx, exp)
	scored := countPointsRows(exp)
	t.Logf("points probe: %d frames, game version %s, %d of %d rows carry hand-checked points",
		len(frames), exp.GameVersion, scored, len(exp.Rows))
	if scored == 0 {
		t.Skipf("no row in the fixture carries a points value; nothing to measure")
	}

	for _, psm := range pointsPSMList(t, engine.PSM) {
		engine.PSM = psm
		for _, cfg := range pointsConfigs() {
			spec := vsPointsSpec
			if *pointsCharset != "" {
				spec.Charset = *pointsCharset
			}
			for _, fb := range pointsFallbackConfigs() {
				// The primary plan is always the shipped setting (psm sweep
				// aside); the retry plan is what -points.fbsweep varies. A
				// bare `make probe-points` measures exactly what IngestVS
				// does: readFieldWithRetry with vsPointsRetry, firing only on
				// an empty primary read, so sweeping fb.opts here can only
				// ever move bands that were empty at the primary -- the same
				// property that made the name probe's -probe.fbsweep a valid
				// apples-to-apples comparison rather than a sweep over every
				// band regardless of whether the retry ever touches it.
				retry := readPlan{spec: ocr.Spec{MinConf: spec.MinConf, PSM: *pointsFallbackPSM}, opts: fb.opts}
				label := fmt.Sprintf("psm%d %s", psm, cfg.label)
				if *pointsFBSweep {
					label = fmt.Sprintf("fb:%-13s", fb.label)
				}
				res := scorePointsConfig(ctx, t, engine, frames, exp, spec, cfg.opts, retry, label)

				t.Logf("%-22s  %2d/%d rows exact  %3d/%d bands exact  %3d within 1%%  %3d unparseable  %3d empty  %3d low-conf",
					res.Label, res.rowsExact(), scored, res.Exact, res.TotalBand,
					res.Within1Pct, res.Unparseable, res.Empty, res.LowConf)

				if *pointsDetail {
					// Printed before the per-row list because it is the number a
					// gate result is read against; the list explains it.
					t.Logf("%-22s  %2d/%d rows had ONE band yielding both an accepted name and exact points "+
						"— the condition IngestVS actually writes on",
						res.Label, len(res.Joint), scored)
					printPointsDetail(t, exp, res)
				}
			}
		}
	}
}

// pointsFallbackConfigs is what the retry preprocesses with: the shipped
// vsPointsRetry options (which today equal vsPointsOptions) unless
// -points.fbsweep asks for the whole grid. Mirrors the name probe's
// fallbackConfigs -- inheriting the primary's options was an assumption there
// too, and sweeping it separately was worth two members.
func pointsFallbackConfigs() []probeConfig {
	if !*pointsFBSweep {
		return []probeConfig{{label: "shipped-retry", opts: vsPointsRetry.opts}}
	}
	return probeShapeGrid()
}

// pointsConfigs is the shipped options unless -points.sweep asks for the full
// grid. The grid is probeShapeGrid, shared with the name probe so a result here
// is directly comparable with one there -- the two fields sit on the same row,
// under the same lighting, and a shape that helps one and hurts the other is a
// real finding rather than an artefact of two separate sweeps.
func pointsConfigs() []probeConfig {
	if !*pointsSweep {
		return []probeConfig{{label: "shipped", opts: vsPointsOptions}}
	}
	return probeShapeGrid()
}

func pointsPSMList(t *testing.T, def int) []int {
	t.Helper()
	if *pointsPSMs == "" {
		return []int{def}
	}
	return parsePSMList(t, *pointsPSMs)
}

// scorePointsConfig reads the points field of every band of every frame under
// one configuration and scores the parsed values against the hand-checked ones.
func scorePointsConfig(ctx context.Context, t *testing.T, engine *ocr.TesseractEngine, frames []probeLoadedFrame, exp probeCapture, spec ocr.Spec, opts vision.Options, retry readPlan, label string) pointsResult {
	t.Helper()

	res := pointsResult{Label: label, Best: map[int]pointsRead{}, Joint: map[int]bool{}, want: wantByRank(exp)}
	rankByPoints := rankByPointsIndex(exp)
	rankByName := rankByNameIndex(exp)

	// The name plan is only built under -points.detail. Reading the name costs
	// as much as reading the points, so a default run does not pay for a field
	// it does not report.
	var members []roster.Member
	var nameByID map[int64]string
	if *pointsDetail {
		members = probeMembers(exp)
		nameByID = make(map[int64]string, len(members))
		for _, m := range members {
			nameByID[m.ID] = m.Name
		}
	}

	ing := &Ingester{engine: engine}
	for _, f := range frames {
		bands, err := SegmentRows(f.Img, vsListRegion, vsRowPitch)
		if err != nil {
			t.Fatalf("segmenting frame %d: %v", f.Seq, err)
		}
		for _, band := range bands {
			res.TotalBand++

			rect := fieldRect(band, f.Img, vsPointsXFrac0, vsPointsXFrac1, vsPointsYFrac0, vsPointsYFrac1)
			// The production read path, retry and all, so the probe cannot
			// drift from what IngestVS does -- the same discipline the name
			// probe follows. readFieldWithRetry fires the retry only on an
			// empty primary read, so a band that already reads at the
			// primary is untouched by anything -points.fbsweep varies.
			read, _, err := ing.readFieldWithRetry(ctx, f.Img, rect, readPlan{spec: spec, opts: opts}, retry)
			if err != nil {
				t.Fatalf("reading frame %d band %d: %v", f.Seq, band.Y0, err)
			}

			cur := pointsRead{
				Text:  strings.TrimSpace(read.Text),
				Conf:  read.Confidence,
				Frame: f.Seq,
				Y0:    band.Y0,
			}
			if *pointsDetail {
				cur.Name = bandName(ctx, t, ing, f, band, members, nameByID)
			}

			exact := false
			switch {
			case cur.Text == "":
				res.Empty++
			default:
				value, perr := ParsePoints(cur.Text)
				if perr != nil {
					res.Unparseable++
					break
				}
				cur.Value, cur.Parsed = value, true

				// Confidence is counted but does not disqualify the read from
				// the accuracy columns. "Did the engine see the right digits"
				// and "would the pipeline have trusted it" are separate
				// questions, and collapsing them would hide a field that reads
				// perfectly and is discarded by a threshold -- a one-constant
				// fix rather than a crop problem.
				if read.Confidence < spec.MinConf {
					res.LowConf++
				}
				if rankByPoints[value] != 0 {
					exact = true
					res.Exact++
					res.Within1Pct++
				} else if within1Pct(value, res.want) != 0 {
					res.Within1Pct++
				}
			}

			// The joint condition, evaluated on this band alone: both fields
			// succeeded here, which is what IngestVS requires and what neither
			// headline number can express.
			if exact && cur.Name != "" && res.want[rankByName[cur.Name]] == cur.Value {
				res.Joint[rankByName[cur.Name]] = true
			}

			if !exact && *pointsDump != "" {
				// dumpEmptyBand is the name probe's dumper; it is named for the
				// case that motivated it but writes the same two images for any
				// band, which is exactly what is wanted here.
				dumpEmptyBand(t, *pointsDump, f, band, rect, opts)
			}
			res.note(rankByPoints, rankByName, cur)
		}
	}
	return res
}

// note files a read against the row it best evidences.
//
// Attribution runs name-first, because a resolved name is ground truth about
// which row a band is while a value match is only strong evidence -- and a name
// is the only thing that can file an empty or unparseable read at all. Failing
// that, an exactly-matching value names its own row. Failing both, a parsed
// value is filed against whichever row it is relatively closest to, so a broken
// row is reported by its nearest miss rather than by an arbitrary band. A read
// with no name and no parsed value cannot be attributed and is counted in the
// aggregates only.
func (r *pointsResult) note(rankByPoints map[int64]int, rankByName map[string]int, cur pointsRead) {
	rank := 0
	switch {
	case cur.Name != "" && rankByName[cur.Name] != 0:
		rank = rankByName[cur.Name]
	case cur.Parsed && rankByPoints[cur.Value] != 0:
		rank = rankByPoints[cur.Value]
	case cur.Parsed:
		rank = r.nearestRank(cur.Value)
	}
	if rank == 0 {
		return
	}
	if prev, ok := r.Best[rank]; !ok || betterRead(cur, prev, r.want[rank]) {
		r.Best[rank] = cur
	}
}

// nearestRank is the row whose points a parsed value is relatively closest to.
// It exists so a misread lands next to the row it most plausibly belongs to
// rather than nowhere, which is what makes the detail view able to say "rank 38
// read 8,835,180 as 8,895,180" instead of merely "rank 38: nothing".
func (r *pointsResult) nearestRank(value int64) int {
	best, bestErr := 0, math.Inf(1)
	for rank, want := range r.want {
		e := relative(value, want)
		if e < bestErr {
			best, bestErr = rank, e
		}
	}
	return best
}

// rowsExact counts rows whose best read parsed to exactly the hand-checked
// value. This is the row-level analogue of the name probe's "distinct members".
func (r *pointsResult) rowsExact() int {
	n := 0
	for rank, want := range r.want {
		if b, ok := r.Best[rank]; ok && b.Parsed && b.Value == want {
			n++
		}
	}
	return n
}

// betterRead ranks one read against another for the same row: an exact value
// beats an inexact one, a parsed read beats an unparsed one, and among inexact
// parsed reads the smaller relative error wins. Confidence deliberately does
// not enter -- Best is about what the engine saw, and a high-confidence wrong
// read should never displace a low-confidence right one in the view whose job
// is to show what the pixels say.
func betterRead(cur, prev pointsRead, want int64) bool {
	return readTier(cur, want) < readTier(prev, want) ||
		(readTier(cur, want) == readTier(prev, want) && relErr(cur, want) < relErr(prev, want))
}

func readTier(r pointsRead, want int64) int {
	switch {
	case r.Parsed && r.Value == want:
		return 0
	case r.Parsed:
		return 1
	case r.Text != "":
		return 2 // read something, could not parse it
	}
	return 3 // empty
}

func relErr(r pointsRead, want int64) float64 {
	if !r.Parsed {
		return math.Inf(1)
	}
	return relative(r.Value, want)
}

func relative(got, want int64) float64 {
	if want == 0 {
		return math.Inf(1)
	}
	return math.Abs(float64(got-want)) / float64(want)
}

// within1Pct returns the rank of a row the value lands within 1% of, or 0. One
// percent is the gate's own tolerance, reported here so a run of this probe can
// be read directly against a gate result rather than through a conversion.
func within1Pct(value int64, want map[int]int64) int {
	for rank, w := range want {
		if relative(value, w) <= 0.01 {
			return rank
		}
	}
	return 0
}

// bandName reads the band's name field through the production plan and returns
// the member it auto-accepts as, or "" if none does.
func bandName(ctx context.Context, t *testing.T, ing *Ingester, f probeLoadedFrame, band RowBand, members []roster.Member, nameByID map[int64]string) string {
	t.Helper()

	rect := fieldRect(band, f.Img, vsNameXFrac0, vsNameXFrac1, vsNameYFrac0, vsNameYFrac1)
	read, _, err := ing.readFieldWithRetry(ctx, f.Img, rect, vsName, vsNameRetry)
	if err != nil {
		t.Fatalf("reading name of frame %d band %d: %v", f.Seq, band.Y0, err)
	}
	text := strings.TrimSpace(read.Text)
	if text == "" {
		return ""
	}
	cands := roster.Rank(text, members)
	if len(cands) == 0 || cands[0].Score < roster.AutoAccept {
		return ""
	}
	return nameByID[cands[0].MemberID]
}

// printPointsDetail lists every row the configuration did not read cleanly,
// worst first, with the read that came closest.
//
// The verdicts separate fixes that have nothing to do with each other. EMPTY
// and UNPARSED are crop or preprocessing problems and belong to this field.
// MISREAD is a digit the engine got wrong. LOWCONF is a read that was right and
// would have been discarded, which is a threshold decision rather than a vision
// one. NONAME marks a row whose points read exactly but whose *name* never
// resolved -- not a points failure at all, and the reason this view carries a
// name column. NOBAND means no band was ever attributed to the row.
func printPointsDetail(t *testing.T, exp probeCapture, res pointsResult) {
	t.Helper()

	type line struct {
		row     probeRow
		read    pointsRead
		ok      bool
		verdict string
	}
	var missed []line
	for _, row := range exp.Rows {
		if row.Points == 0 {
			continue
		}
		read, ok := res.Best[row.Rank]
		v := pointsVerdict(read, ok, row)
		if v == "OK" {
			continue
		}
		missed = append(missed, line{row: row, read: read, ok: ok, verdict: v})
	}
	sort.Slice(missed, func(i, j int) bool {
		return relErr(missed[i].read, missed[i].row.Points) > relErr(missed[j].read, missed[j].row.Points)
	})

	t.Logf("--- %s: %d rows not read cleanly ---", res.Label, len(missed))
	for _, m := range missed {
		t.Logf("  rank %3d  %-22q want %11s  got %11s  conf %.2f  %-8s read %q (frame %d)",
			m.row.Rank, m.row.Name, group(m.row.Points), gotValue(m.read, m.ok),
			m.read.Conf, m.verdict, m.read.Text, m.read.Frame)
	}
}

func pointsVerdict(read pointsRead, ok bool, row probeRow) string {
	switch {
	case !ok:
		return "NOBAND"
	case read.Text == "":
		return "EMPTY"
	case !read.Parsed:
		return "UNPARSED"
	case read.Value != row.Points:
		return "MISREAD"
	case read.Conf < vsPointsSpec.MinConf:
		return "LOWCONF"
	case read.Name != row.Name:
		// Only meaningful under -points.detail, which is the only mode that
		// populates Name and the only mode this function runs in.
		return "NONAME"
	}
	return "OK"
}

func gotValue(r pointsRead, ok bool) string {
	if !ok || !r.Parsed {
		return "-"
	}
	return group(r.Value)
}

// group renders a points value the way the screen does, so a hand-checked
// number and a raw read can be compared glyph by glyph without mental
// arithmetic -- which is where a transposed digit hides.
func group(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func wantByRank(exp probeCapture) map[int]int64 {
	out := make(map[int]int64, len(exp.Rows))
	for _, row := range exp.Rows {
		if row.Points != 0 {
			out[row.Rank] = row.Points
		}
	}
	return out
}

// rankByPointsIndex maps a hand-checked points value back to its rank. A
// duplicate value maps to 0 and is therefore never treated as an exact
// attribution: two members on the same total are indistinguishable by this
// field, and guessing between them is the false-attribution failure the name
// probe's closest-pair check exists to prevent.
func rankByPointsIndex(exp probeCapture) map[int64]int {
	out := make(map[int64]int, len(exp.Rows))
	dup := map[int64]bool{}
	for _, row := range exp.Rows {
		if row.Points == 0 {
			continue
		}
		if _, seen := out[row.Points]; seen {
			dup[row.Points] = true
			continue
		}
		out[row.Points] = row.Rank
	}
	for v := range dup {
		out[v] = 0
	}
	return out
}

func rankByNameIndex(exp probeCapture) map[string]int {
	out := make(map[string]int, len(exp.Rows))
	for _, row := range exp.Rows {
		out[row.Name] = row.Rank
	}
	return out
}

func countPointsRows(exp probeCapture) int {
	n := 0
	for _, row := range exp.Rows {
		if row.Points != 0 {
			n++
		}
	}
	return n
}

// parsePSMList is shared with the name probe's -probe.psm.
func parsePSMList(t *testing.T, spec string) []int {
	t.Helper()
	var out []int
	for _, s := range strings.Split(spec, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			t.Fatalf("page-segmentation mode %q is not a number: %v", s, err)
		}
		out = append(out, n)
	}
	return out
}
