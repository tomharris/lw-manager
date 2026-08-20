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
// It grew five more instruments once the roster gate started failing, because
// the name column was the only field on this route that had one and the name
// column turned out not to be where the damage was:
//
//	make probe-roster PROBE_ARGS='-roster.badge'          # the rank NCC read
//	make probe-roster PROBE_ARGS='-roster.header'         # the group header OCR
//	make probe-roster PROBE_ARGS='-roster.headerink'      # the header histogram
//	make probe-roster PROBE_ARGS='-roster.headersweep'    # the header crop's edges
//	make probe-roster PROBE_ARGS='-roster.headeropts'     # preprocessing + PSM, through both candidate rects
//	make probe-roster PROBE_ARGS='-roster.headerthresh'   # AdaptiveThreshold's block size and C
//	make probe-roster PROBE_ARGS='-roster.power'          # the power column
//	make probe-roster PROBE_ARGS='-roster.level'          # the level column
//	make probe-roster PROBE_ARGS='-roster.members'        # per MEMBER, not per band
//
// The last three score against the transcribed group TOTALS, not merely
// against whether a header parsed: parseGroupHeader's doc comment records that
// an under-count is the failure that does silent damage, so an instrument that
// counted parses alone would rank a fabrication above a refusal.
//
// -roster.badgeshuffle is not a sixth instrument: it rotates -roster.badge's
// own truth table so every verdict is wrong by construction, which validates
// that mode rather than measuring anything new. It implies -roster.badge.
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
// It scores against fixtures/m4rostergate/expected.yaml: 75 members, this
// capture, transcribed frame by frame at full resolution. That file did not
// exist when this probe was written -- it scored against the VS ranking's 86
// ranked names instead, three days later and missing 11 of this roster's
// members -- so every number here used to carry a caveat that `exact` was a
// lower bound and `junk-prefixed` was the only column that meant anything.
// The caveat is gone. `exact` is an accuracy, `unmatched` is an error rate,
// and a member missing from the per-member view is missing from the capture's
// reads rather than possibly missing from the truth set.
//
// Two columns still need their own reading:
//
//   - `junk-prefixed` counts reads that match a name only after one leading
//     token is stripped. It is the direct measure of a left-edge crop defect
//     (a status icon read as a first character) and it is the column a crop
//     change is read against first. It is 0 today.
//   - `exact (below MinConf)` counts reads that are byte-identical to a
//     transcribed name and BELOW nameSpec.MinConf, which processRow refuses to
//     create a member from. A pure accuracy count hides those completely: the
//     name was read correctly and the member was dropped anyway.
//
// -roster.members is the per-MEMBER view, and it is the one that answers the
// gate's question. See its own doc comment for why a per-band count cannot.
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
	"image/color"
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
	"github.com/tomharris/lw-manager/internal/roster"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

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
	rosterNoRetry = flag.Bool("roster.noretry", false,
		"read the name at PSM 7 only, without the PSM 13 retry production ships; this is what the retry is worth, measured, not the shipped configuration")
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
	rosterHeaderSweep = flag.Bool("roster.headersweep", false,
		"sweep the header crop's edges across the measured chevron gutter, and the count-only crop beside it, scoring the parsed total against the transcribed group size")
	rosterHeaderOptions = flag.Bool("roster.headeropts", false,
		"the eight-shape x three-upscale preprocessing grid through BOTH candidate header rectangles; slow, and the re-measurement moving the crop makes necessary")
	rosterHeaderThresh = flag.Bool("roster.headerthresh", false,
		"sweep AdaptiveThreshold's block size and C through both candidate header rectangles; the count that resists every skip-flag shape is a contrast failure, not a layout one, and these are the only two knobs the shape grid never varies")
	rosterBadgeShuffle = flag.Bool("roster.badgeshuffle", false,
		"rotate every truth label one rank forward, so the badge mode's verdicts are wrong by construction; it must report ~0 agree. Implies -roster.badge")
	rosterLastActive = flag.Bool("roster.lastactive", false,
		"report what the last-active field reads per band, split by whether the transcribed value is the green \"Online\" or a grey elapsed time, through the shipped luma grayscale and through each colour channel")
	rosterLASweep = flag.Bool("roster.lasweep", false,
		"sweep the status crop's preprocessing over the bands whose transcribed value is the green \"Online\": 8 skip-flag shapes x 3 upscales x luma/green-channel, plus PSM 8 and 13. Implies -roster.lastactive")
	rosterMembers = flag.Bool("roster.members", false,
		"per-MEMBER view: each transcribed member's best band, and whether processRow would match it, create from it, or refuse it for confidence")
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

// rosterBandRead is one row band's name read, kept so the sweep, the detail
// view and the per-member report can be built from a single pass over the
// pixels.
//
// Conf and Retried are here because the name's OCR confidence is not
// decoration on this route: processRow enforces nameSpec.MinConf on the
// member-CREATION branch, so a band read perfectly at confidence 0.39 mints
// nobody. A per-band report that showed only the text could not distinguish
// "read wrong" from "read right and refused", and those need opposite fixes.
type rosterBandRead struct {
	Seq     int
	Y0      int
	Text    string
	Conf    float64
	Retried bool
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
	// Unmatched reads that matched nothing either way. Against the roster's
	// own transcription this IS an error rate: every member of this capture
	// is in the truth set, so a read matching nothing is a read that is
	// wrong. (It was not one against the VS fixture, which lacked 11 of these
	// members outright -- see rosterTruth.)
	Unmatched int
	// ExactBelowMinConf is the subset of Exact that processRow would refuse
	// to create a member from: the read is byte-identical to a transcribed
	// name and the engine's own confidence is below nameSpec.MinConf. It is
	// broken out because it is the one failure an accuracy count actively
	// hides -- the pipeline read the name correctly and dropped it anyway,
	// which looks like a success in every column but this one.
	ExactBelowMinConf int
}

func (c rosterCounts) String() string {
	return fmt.Sprintf("%-22s bands %3d  exact %3d (below MinConf %2d)  junk-prefixed %3d  empty %3d  unmatched %3d",
		c.Label, c.Bands, c.Exact, c.ExactBelowMinConf, c.JunkPrefixed, c.Empty, c.Unmatched)
}

func TestRosterNameProbe(t *testing.T) {
	ctx := context.Background()

	truth := loadRosterTruth(t)
	frames := loadRosterFrames(t, ctx)
	t.Logf("roster probe: %d member-list frames, %d hand-transcribed names to score against",
		len(frames), len(truth.Names))
	t.Logf("  the truth set is fixtures/m4rostergate/expected.yaml -- THIS capture, transcribed")
	t.Logf("  frame by frame. `exact` is an accuracy now, not the lower bound it was against")
	t.Logf("  the VS ranking's 86 scorers. `unmatched` is a read that is wrong.")

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
	if *rosterBadge || *rosterBadgeShuffle {
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

	if *rosterHeaderSweep || *rosterHeaderOptions || *rosterHeaderThresh {
		if *rosterHeaderSweep {
			reportRosterHeaderSweep(ctx, t, engine, frames)
		}
		if *rosterHeaderOptions {
			reportRosterHeaderOptions(ctx, t, engine, frames)
		}
		if *rosterHeaderThresh {
			reportRosterHeaderThreshold(ctx, t, engine, frames)
		}
		return
	}

	if *rosterLastActive || *rosterLASweep {
		reportRosterLastActive(ctx, t, engine, frames, truth)
		return
	}

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
		if *rosterMembers {
			reportRosterMembers(t, reads, truth)
		}
	}
}

// readRosterNames runs the production read path over every row band of every
// frame at the given left edge. No dedupe: a member visible in three
// overlapping frames is scored three times, so one bad band reads differently
// from a systematic miss — the same choice zz_name_probe_test.go makes and for
// the same reason.
//
// "Production" means production: readFieldWithRetry against the shipped
// nameRetry, because processRow ships that retry. It did not when this mode
// was written, and the flag inverted with it -- what used to be -roster.retry
// ("what would the retry buy?") is now -roster.noretry ("what is the retry
// worth, measured against its own absence?"). A probe whose default models a
// weaker pipeline than the one that ships reports failures production does not
// have, which CLAUDE.md records costing a correct change its acceptance on the
// VS route.
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
			var retried bool
			var err error
			if *rosterNoRetry {
				res, err = ing.readField(ctx, f.Img, rect, nameSpec, nameOptions)
			} else {
				res, retried, err = ing.readFieldWithRetry(ctx, f.Img, rect,
					readPlan{spec: nameSpec, opts: nameOptions}, nameRetry)
			}
			if err != nil {
				t.Fatalf("reading frame %d band %d: %v", f.Seq, band.Y0, err)
			}
			out = append(out, rosterBandRead{
				Seq: f.Seq, Y0: band.Y0,
				Text: strings.TrimSpace(res.Text), Conf: res.Confidence, Retried: retried,
			})
		}
	}
	return out
}

func scoreRosterReads(reads []rosterBandRead, truth rosterTruth, label string) rosterCounts {
	c := rosterCounts{Label: label, Bands: len(reads)}
	for _, r := range reads {
		switch {
		case r.Text == "":
			c.Empty++
		case truth.Names[r.Text]:
			c.Exact++
			if r.Conf < nameSpec.MinConf {
				c.ExactBelowMinConf++
			}
		case stripLeadingToken(r.Text) != r.Text && truth.Names[stripLeadingToken(r.Text)]:
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

// reportDistinctReads prints the most common raw reads and their counts, most
// frequent first. Every mode in this file that reports "N distinct reads" used
// to discard the distribution behind that count -- the most actionable numbers
// in the whole probe (which reads are common, which are one-offs) were only
// ever hand-derived from the per-band log lines, and were not reproducible
// from the committed instrument itself. topN <= 0 prints all of them, which is
// appropriate for a field whose distinct count is already small (the header).
func reportDistinctReads(t *testing.T, label string, distinct map[string]int, topN int) {
	t.Helper()
	type kv struct {
		Text  string
		Count int
	}
	kvs := make([]kv, 0, len(distinct))
	for text, n := range distinct {
		kvs = append(kvs, kv{text, n})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].Count != kvs[j].Count {
			return kvs[i].Count > kvs[j].Count
		}
		return kvs[i].Text < kvs[j].Text
	})
	shown := kvs
	if topN > 0 && len(shown) > topN {
		shown = shown[:topN]
	}
	parts := make([]string, 0, len(shown))
	for _, e := range shown {
		parts = append(parts, fmt.Sprintf("%d %q", e.Count, e.Text))
	}
	suffix := ""
	if len(shown) < len(kvs) {
		suffix = fmt.Sprintf("  (top %d of %d distinct)", len(shown), len(kvs))
	}
	t.Logf("  %s reads: %s%s", label, strings.Join(parts, "  "), suffix)
}

func reportRosterDetail(t *testing.T, reads []rosterBandRead, truth rosterTruth) {
	t.Helper()
	t.Log("  per band:")
	for _, r := range reads {
		verdict := "unmatched"
		switch {
		case r.Text == "":
			verdict = "EMPTY"
		case truth.Names[r.Text]:
			verdict = "exact"
		case stripLeadingToken(r.Text) != r.Text && truth.Names[stripLeadingToken(r.Text)]:
			verdict = fmt.Sprintf("JUNK-PREFIXED -> %q", stripLeadingToken(r.Text))
		}
		best := ""
		if cands := roster.Rank(r.Text, truth.Members); len(cands) > 0 && cands[0].Score > 0 {
			best = fmt.Sprintf(" best=%q@%d", cands[0].Name, cands[0].Score)
		}
		retried := ""
		if r.Retried {
			retried = " RETRIED"
		}
		t.Logf("    frame %2d y=%4d  read=%-28q conf=%.2f%s %s%s",
			r.Seq, r.Y0, r.Text, r.Conf, retried, verdict, best)
	}
}

// reportRosterMembers is the per-MEMBER view: one line per transcribed member,
// naming the band that came closest to reading them and what processRow would
// have done with it.
//
// It exists because the band-keyed views above cannot answer the question the
// gate actually poses. `make gate-roster` reports members never created; a
// per-band report reports reads that matched nothing; and pairing those two
// aggregates is exactly the inference CLAUDE.md records this milestone getting
// wrong twice ("two aggregates side by side are not a causal claim" -- 15
// empty bands beside 10 non-Latin names, which were disjoint sets). This mode
// keys on the member, so a missing member names its own best read and its own
// refusal instead of being matched to one by hand.
//
// The verdict column models processRow's routing, and models it against a
// roster that already contains every member -- which production does NOT have
// on a first ingest, where members are created as the scan proceeds. So:
//
//   - MATCH means some band scores >= roster.AutoAccept against this member,
//     so once the member exists, this row finds them. On a first ingest that
//     is the SECOND and later sightings.
//   - CREATABLE means the best band is both AutoAccept-worthy and above
//     nameSpec.MinConf, so the first sighting could have minted the member.
//   - LOW-CONF means the read is good enough to match but too weak to create
//     from: a member reachable only if somebody else creates them first, which
//     on a first ingest means never.
//   - MISS means no band on any frame scores AutoAccept against this member at
//     all, whatever the confidence.
//
// The distinction between CREATABLE and LOW-CONF is the whole reason the mode
// carries confidence, and it is not visible from any band-keyed count.
func reportRosterMembers(t *testing.T, reads []rosterBandRead, truth rosterTruth) {
	t.Helper()

	type bestRead struct {
		read  rosterBandRead
		score int
		found bool
	}
	// Best band per member by score, and among equal scores the one with the
	// highest confidence -- because confidence is what decides creation, and
	// a member with one 0.72 sighting and eleven 0.20 sightings is creatable.
	best := map[string]bestRead{}
	for _, r := range reads {
		if r.Text == "" {
			continue
		}
		for _, m := range truth.Members {
			s := roster.TokenSetRatio(r.Text, m.Name)
			b := best[m.Name]
			if !b.found || s > b.score || (s == b.score && r.Conf > b.read.Conf) {
				best[m.Name] = bestRead{read: r, score: s, found: true}
			}
		}
	}

	byVerdict := map[string]int{}
	names := make([]string, 0, len(truth.Members))
	for _, m := range truth.Members {
		names = append(names, m.Name)
	}
	sort.Slice(names, func(i, j int) bool {
		if truth.Rank[names[i]] != truth.Rank[names[j]] {
			return truth.Rank[names[i]] > truth.Rank[names[j]]
		}
		return names[i] < names[j]
	})

	t.Log("  per member (best band, and what processRow would do with it):")
	for _, name := range names {
		b := best[name]
		verdict := "MISS"
		switch {
		case !b.found || b.score < roster.AutoAccept:
			verdict = "MISS"
		case b.read.Conf >= nameSpec.MinConf:
			verdict = "CREATABLE"
		default:
			verdict = "LOW-CONF"
		}
		byVerdict[verdict]++
		if !b.found {
			t.Logf("    %-3s %-22q no band read anything at all", truth.Rank[name], name)
			continue
		}
		t.Logf("    %-3s %-22q %-9s score %3d  conf %.2f  frame %2d y=%4d read=%q",
			truth.Rank[name], name, verdict, b.score, b.read.Conf, b.read.Seq, b.read.Y0, b.read.Text)
	}
	t.Logf("  per member: %d members, CREATABLE %d, LOW-CONF %d, MISS %d",
		len(names), byVerdict["CREATABLE"], byVerdict["LOW-CONF"], byVerdict["MISS"])
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
// rosterTruth is the hand-transcribed roster capture, as much of it as the
// name modes need: the set of names for the exact/junk-prefixed scoring, and
// the same names as roster.Members so a band can be scored the way processRow
// scores it.
//
// It is loaded from fixtures/m4rostergate/expected.yaml, which is a
// transcription of THIS capture, frame by frame, at full resolution. Until the
// roster gate existed this probe had no such file and scored against
// fixtures/m4gate/expected.yaml -- the VS ranking's 86 scorers, three days
// later -- so `exact` was a lower bound that could never be quoted as an
// accuracy, and every mode's headline carried that caveat. The roster gate's
// fixture removes the caveat: 75 members, this capture, every one of them
// expected to appear in some band. `exact` is now an accuracy, and a read that
// matches nothing is a miss rather than possibly-a-member-the-truth-set-lacks.
//
// Two consequences worth stating, because they change how the numbers read:
//
//   - the totals move DOWN when the truth set changes, and that is not a
//     regression. The VS set carried 86 names of which many are in this
//     roster; the roster set carries 75, and the two disagree on five of them
//     by hand (the fixture's own header lists them: ALBAN80 not ALBANSO,
//     Bwiz21 not Bwiz2t, and so on). A band whose read matched the VS
//     transcription of a name and not the roster's is now a miss, correctly.
//   - `Rank` is available, so a band can be scored only against the members of
//     the group its frame belongs to. Nothing does that yet -- the reads are
//     scored against all 75 -- but the per-member report needs the rank to say
//     which group a missing member belongs to.
type rosterTruth struct {
	Names   map[string]bool
	Members []roster.Member
	Rank    map[string]string
	// LastActive is the transcribed status string ("Online", "3h ago") per
	// member name -- the ground truth for -roster.lastactive.
	LastActive map[string]string
}

func loadRosterTruth(t *testing.T) rosterTruth {
	t.Helper()

	path := filepath.Join("..", "..", "fixtures", "m4rostergate", "expected.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no hand-checked names at %s", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var exp struct {
		Members []struct {
			Rank       string `yaml:"rank"`
			Name       string `yaml:"name"`
			LastActive string `yaml:"last_active"`
		} `yaml:"members"`
	}
	if err := yaml.Unmarshal(data, &exp); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	out := rosterTruth{
		Names:      make(map[string]bool, len(exp.Members)),
		Members:    make([]roster.Member, 0, len(exp.Members)),
		Rank:       make(map[string]string, len(exp.Members)),
		LastActive: make(map[string]string, len(exp.Members)),
	}
	for i, m := range exp.Members {
		out.Names[m.Name] = true
		// The ID is the fixture's own index, not a members-table id: this
		// probe has no database and never writes one. It exists so
		// roster.Rank's Candidate can be tied back to a truth row.
		out.Members = append(out.Members, roster.Member{ID: int64(i + 1), Name: m.Name})
		out.Rank[m.Name] = m.Rank
		out.LastActive[m.Name] = m.LastActive
	}
	if len(out.Names) == 0 {
		t.Fatalf("%s carries no members", path)
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
	distinctScores := map[string]int{}

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
		distinctScores[fmt.Sprintf("%.3f/%.3f", best.score, runnerUp.score)]++
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
	t.Logf("  badge: %d distinct (best,runner-up) score pairs over %d frames -- the badge sprite is a",
		len(distinctScores), len(frames))
	t.Log("         static asset at a fixed screen position, so a small number here is expected " +
		"NCC finding the same placement repeatedly, not the instrument returning a constant")

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

// headerRankToken pulls the first rank-shaped token ("R1".."R4") out of a raw
// header read, so the read can be cross-checked against rosterFrameRanks on a
// signal that is already sitting in the text -- the header carries its own
// rank ("(R3)", "[R4)") even on reads whose N/M count is destroyed. That turns
// rosterFrameRanks from a hand-built table validated only by eye and by the
// colour cross-check into one corroborated by the OCR engine on every run.
var headerRankToken = regexp.MustCompile(`R\d`)

// reportRosterHeader reads groupHeaderRegion on every frame and reports the
// raw text, what parseGroupHeader made of it, and why it refused.
//
// A header that will not parse makes IngestRoster `continue` before
// SegmentRows is ever called, so the whole frame is dropped. That is how the
// chevron-in-the-crop defect was found: the review queue's raw text named it,
// "R2) I'm Alright VN iy]" keeping the group name and losing the N/M count
// while X2 sat at 0.97, past the collapse chevron. Moving the edge into the
// measured gutter took this mode from 39 parsed to 40 and from 42 of 61 reads
// below groupHeaderSpec.MinConf to none -- and left the other 21 exactly where
// they were, because R2's "1/11" is a classifier failure this crop cannot
// reach (see groupHeaderRegion's own comment, and -roster.headerthresh).
//
// It counts identical reads and prints the most common ones at the end,
// because this file's own warning applies here more than anywhere: a header
// mode that reported one string for every frame would be measuring a
// constant, not a crop. The frames show three different groups, so the
// printed reads are the tell (CLAUDE.md, "A broken instrument reports
// agreement, not noise") and are reproducible from this instrument's own
// output rather than hand-derived from the per-frame lines above them.
func reportRosterHeader(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame) {
	t.Helper()
	ing := New(nil, nil, engine)
	truth := rosterFrameRanks()

	ok, bad := 0, 0
	lowConf := 0
	distinct := map[string]int{}
	byReason := map[string]int{}
	rankAgree, rankDisagree, rankAbsent := 0, 0, 0
	for _, f := range frames {
		want := truth[f.Seq]
		res, err := ing.readField(ctx, f.Img, groupHeaderRegion, groupHeaderSpec, groupHeaderOptions)
		if err != nil {
			t.Logf("  seq %2d  want %-3s  READ ERROR %v", f.Seq, want, err)
			bad++
			continue
		}
		distinct[res.Text]++
		if !res.Accepted(groupHeaderSpec) {
			lowConf++
		}
		if tok := headerRankToken.FindString(res.Text); tok == "" {
			rankAbsent++
		} else if tok == want {
			rankAgree++
		} else {
			rankDisagree++
			t.Logf("  seq %2d  want %-3s  RANK TOKEN DISAGREES: raw text carries %q", f.Seq, want, tok)
		}
		name, total, perr := parseGroupHeader(res.Text)
		if perr != nil {
			bad++
			byReason[rosterHeaderRefusalReason(perr)]++
			t.Logf("  seq %2d  want %-3s  conf %.2f  %-44q  REFUSED: %v", f.Seq, want, res.Confidence, res.Text, perr)
			continue
		}
		ok++
		t.Logf("  seq %2d  want %-3s  conf %.2f  %-44q  -> %q total=%d", f.Seq, want, res.Confidence, res.Text, name, total)
	}
	t.Logf("  header raw-text rank token cross-check: %d agree, %d disagree, %d carried no R-token, of %d frames",
		rankAgree, rankDisagree, rankAbsent, len(frames))
	t.Logf("  header: %d parsed, %d refused, of %d frames (%d read below groupHeaderSpec.MinConf=%.2f)",
		ok, bad, len(frames), lowConf, groupHeaderSpec.MinConf)
	t.Logf("  distinct raw reads: %d over %d frames -- one or two would mean this mode is measuring a constant, not a crop",
		len(distinct), len(frames))
	reportDistinctReads(t, "header", distinct, 0)
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
//
// Two digits are required after the "v", not one: this alliance's levels run
// 30-35, so "v35" is a read with an intact value and only the "L" missing,
// while "v2" or "v3" are missing digits too and would come back as level 2 or
// 3 in an alliance where nothing is below 30 -- restoring the "L" on those
// would launder a wrong value into a well-formed one, which is the opposite
// of what this shape is supposed to identify. A single-digit read like that
// falls into the "other" bucket instead.
var levelLostLeadingL = regexp.MustCompile(`^[vV]\.?\s*\d\d`)

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
	// Each shape's own regex is printed alongside its count rather than a fixed
	// sentence describing what the shape means: a shape whose regex has drifted
	// (or was too loose to begin with -- see levelLostLeadingL's history) is
	// still visible from this line, where a hardcoded narrative would keep
	// asserting the old claim after the code no longer measured it.
	for _, sh := range fp.Shapes {
		t.Logf("  %s: %d of the %d refusals match shape %s %s", fp.Name, byShape[sh.Name], refused, sh.Name, sh.Re)
	}
	if len(fp.Shapes) > 0 {
		t.Logf("  %s: %d refusals match none of the shapes above (\"other\")", fp.Name, byShape["other"])
	}
	t.Logf("  %s: %d distinct reads over %d bands -- near-uniformity here would mean this mode is measuring a constant",
		fp.Name, len(distinct), bands)
	reportDistinctReads(t, fp.Name, distinct, 25)
}

// ---------------------------------------------------------------------------
// The header crop sweep: which rectangle, and which preprocessing through it.
// ---------------------------------------------------------------------------

// rosterGroupTotals is the rank -> group size table, read out of the roster
// gate's hand-transcribed fixture rather than restated here. It is what turns
// the header sweep from "did it parse" into "did it parse the right number",
// which is the only version of the question worth asking: parseGroupHeader's
// own doc comment records that an UNDER-count is the failure that does silent
// damage (a fabricated 6 against a real 64-member group stops the other 58
// members being created), so a sweep that counted parses alone would rank a
// fabrication above a refusal.
//
// It is deliberately not hand-built the way rosterFrameRanks above is: the
// numbers exist in a committed fixture, and copying them into a test file
// would be one more table to drift.
func rosterGroupTotals(t *testing.T) map[string]int {
	t.Helper()
	path := filepath.Join("..", "..", "fixtures", "m4rostergate", "expected.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no roster transcription at %s", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc struct {
		Groups []struct {
			Rank  string `yaml:"rank"`
			Total int    `yaml:"total"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	out := make(map[string]int, len(doc.Groups))
	for _, g := range doc.Groups {
		out[g.Rank] = g.Total
	}
	if len(out) == 0 {
		t.Fatalf("%s carries no groups block; the sweep has nothing to score against", path)
	}
	return out
}

// headerShape is one candidate way of reading the sticky group header: a
// rectangle, an ocr.Spec and a vision.Options. The three travel together for
// the reason readPlan's doc comment gives and CLAUDE.md states outright --
// options measured through the wrong rectangle are not evidence about the
// right one -- so the sweep never varies preprocessing without naming the crop
// it was measured through.
type headerShape struct {
	label string
	rect  transport.Rect
	spec  ocr.Spec
	opts  vision.Options
}

// headerSweepCounts is one shape's result. Parsed and Refused answer "did
// parseGroupHeader accept it"; Correct and Wrong answer "was the number right",
// scored against the fixture's own group totals. The second pair is the one
// that decides anything -- see rosterGroupTotals above.
type headerSweepCounts struct {
	Label    string
	Frames   int
	Parsed   int
	Refused  int
	Correct  int
	Wrong    int
	Distinct int
}

func (c headerSweepCounts) String() string {
	return fmt.Sprintf("%-34s frames %3d  parsed %3d  refused %3d  total correct %3d  WRONG %3d  distinct reads %3d",
		c.Label, c.Frames, c.Parsed, c.Refused, c.Correct, c.Wrong, c.Distinct)
}

// scoreHeaderShape reads every frame's header through one shape and scores the
// parsed count against the transcribed group size for that frame's rank.
//
// Wrong totals are logged per frame rather than only counted: a shape that
// parses 61 of 61 and gets one number wrong is worse than one that parses 39
// and gets them all right, and only the per-frame line says which frame to go
// and look at (CLAUDE.md, "Two aggregates side by side are not a causal
// claim").
func scoreHeaderShape(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame, ranks map[int]string, totals map[string]int, sh headerShape) headerSweepCounts {
	t.Helper()
	ing := New(nil, nil, engine)
	out := headerSweepCounts{Label: sh.label, Frames: len(frames)}
	distinct := map[string]int{}
	for _, f := range frames {
		res, err := ing.readField(ctx, f.Img, sh.rect, sh.spec, sh.opts)
		if err != nil {
			t.Fatalf("%s: seq %d: %v", sh.label, f.Seq, err)
		}
		distinct[res.Text]++
		_, total, perr := parseGroupHeader(res.Text)
		if perr != nil {
			out.Refused++
			continue
		}
		out.Parsed++
		want, known := totals[ranks[f.Seq]]
		switch {
		case !known:
			t.Logf("    %s: seq %d has no transcribed rank; its total %d is unscored", sh.label, f.Seq, total)
		case total == want:
			out.Correct++
		default:
			out.Wrong++
			t.Logf("    %s: seq %2d (%s) parsed total=%d, want %d, from %q",
				sh.label, f.Seq, ranks[f.Seq], total, want, res.Text)
		}
	}
	out.Distinct = len(distinct)
	// The reads themselves, not only how many there were. A sweep that printed
	// counts alone could show a shape refusing 21 frames without ever saying
	// what those frames read, which is the difference between "the crop still
	// contains something" and "the count is not being seen at all" -- the two
	// diagnoses this file exists to tell apart.
	reportDistinctReads(t, "    "+sh.label, distinct, 8)
	return out
}

// headerCountRegion is the count-only rectangle option (b) of task 6 is
// measured through: the "N/M" token alone, with the group name outside it.
// Placed off -roster.headerink over 61 frames and 2562 scanlines -- the count
// occupies x=565..634 of a 720px frame, with zero ink from x=195..564 to its
// left and the gutter at x=636..650 to its right -- so both edges sit
// in a zero-ink plateau rather than against a glyph.
var headerCountRegion = transport.Rect{X1: 0.7778, Y1: groupHeaderRegion.Y1, X2: 0.8917, Y2: groupHeaderRegion.Y2}

// reportRosterHeaderSweep measures the GEOMETRY: the shipped crop, the crop
// with its right edge walked across the chevron gutter, and the count-only
// crop, all at the shipped preprocessing.
//
// It exists because the header crop is the third crop in this milestone to be
// placed where a human reading the rectangle still saw the right answer, and
// the rule after the second one was to place the next edge off a histogram and
// then re-measure through it rather than reason about it.
func reportRosterHeaderSweep(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame) {
	t.Helper()
	ranks := rosterFrameRanks()
	totals := rosterGroupTotals(t)
	t.Logf("  scoring against the transcribed group totals: %v", totals)

	var shapes []headerShape
	// The whole-header crop, right edge walked from the shipped 0.97 (x=698,
	// past the chevron AND past the card) leftward through the chevron
	// (x=651..683) and into the measured gutter (x=636..650, 0.8833..0.9028;
	// x=635 reads as gutter only at the profile's default threshold of 90 and
	// carries the count's antialiasing tail below it, so the plateau starts one
	// column later than a single-threshold reading suggests).
	// 0.8806 is the count's own last column and is included as the far side of
	// the gutter: a shape that clips a digit should show up as a WRONG total,
	// not merely as a refusal, and that is the failure this column is here to
	// catch.
	for _, x2 := range []float64{0.97, 0.9500, 0.9042, 0.9028, 0.8917, 0.8819, 0.8806, 0.8750} {
		shapes = append(shapes, headerShape{
			label: fmt.Sprintf("header X2=%.4f", x2),
			rect:  transport.Rect{X1: groupHeaderRegion.X1, Y1: groupHeaderRegion.Y1, X2: x2, Y2: groupHeaderRegion.Y2},
			spec:  groupHeaderSpec,
			opts:  groupHeaderOptions,
		})
	}
	// The left edge, at the best right edge. X1=0.03 is x=21, which is OUTSIDE
	// the header card (the card starts at x=28) and admits a strip of page
	// background -- the likeliest source of the leading "{", "(", "[", "|" and
	// "i" on every read. 0.0417 is rankBadgeRegion.X1, inside the card.
	for _, x1 := range []float64{0.0417, 0.0500} {
		shapes = append(shapes, headerShape{
			label: fmt.Sprintf("header X1=%.4f X2=%.4f", x1, groupHeaderRegion.X2),
			rect:  transport.Rect{X1: x1, Y1: groupHeaderRegion.Y1, X2: groupHeaderRegion.X2, Y2: groupHeaderRegion.Y2},
			spec:  groupHeaderSpec,
			opts:  groupHeaderOptions,
		})
	}
	// Option (b): the count on its own, at the shipped preprocessing. Its own
	// options are measured separately by -roster.headeropts, because a
	// rectangle holding only cyan-and-white digits on light blue is not the
	// rectangle groupHeaderOptions was fitted through either.
	for _, x1 := range []float64{0.7778, 0.7847} {
		shapes = append(shapes, headerShape{
			label: fmt.Sprintf("count-only X1=%.4f", x1),
			rect:  transport.Rect{X1: x1, Y1: headerCountRegion.Y1, X2: headerCountRegion.X2, Y2: headerCountRegion.Y2},
			spec:  groupHeaderSpec,
			opts:  groupHeaderOptions,
		})
	}

	for _, sh := range shapes {
		t.Log("  " + scoreHeaderShape(ctx, t, engine, frames, ranks, totals, sh).String())
	}
}

// reportRosterHeaderOptions measures the PREPROCESSING, through both candidate
// rectangles, at the eight skip-flag shapes and three upscale factors that set
// every other option in this package.
//
// groupHeaderOptions' own comment records that every shape including adaptive
// threshold after equalize scored 0-1/18 -- but that was measured through the
// old rectangle, with the chevron in it, and CLAUDE.md says in as many words
// that options measured through the wrong rectangle are not evidence about the
// right one. This is the re-measurement.
//
// What it runs, stated exactly so the total is not double-counted the way a
// bare "48 configurations" invites: 24 shapes (8 skip-flag combinations x 3
// upscale factors) through EACH of the two rectangles, then PSM 8/11/13
// through the count-only rectangle alone, then a "0123456789/" whitelist
// through both. 58 reads of every frame in all, which is why it is its own
// flag.
func reportRosterHeaderOptions(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame) {
	t.Helper()
	ranks := rosterFrameRanks()
	totals := rosterGroupTotals(t)

	rects := []struct {
		name string
		rect transport.Rect
	}{
		{"header-to-gutter", groupHeaderRegion},
		{"count-only", headerCountRegion},
	}
	for _, r := range rects {
		t.Logf("  through %s (X1=%.4f X2=%.4f):", r.name, r.rect.X1, r.rect.X2)
		for _, cfg := range probeShapeGrid() {
			sh := headerShape{
				label: fmt.Sprintf("%s %s", r.name, cfg.label),
				rect:  r.rect,
				spec:  groupHeaderSpec,
				opts:  cfg.opts,
			}
			t.Log("  " + scoreHeaderShape(ctx, t, engine, frames, ranks, totals, sh).String())
		}
	}

	// The page-segmentation modes, through the count-only rectangle only. A
	// short token in a small box is exactly the shape CLAUDE.md records PSM 7's
	// layout analysis going blind to, and PSM 8 ("single word") and PSM 13
	// ("raw line") are the two documented rescues. They are measured here
	// rather than assumed inapplicable because the reads this mode is chasing
	// are WRONG rather than EMPTY, and the retry rule -- confine a raw-line
	// read to crops that produced nothing at all -- is about empty reads. If a
	// PSM changed the answer here it would be evidence for a different
	// mechanism than contrast.
	for _, psm := range []int{8, 11, ocr.PSMRawLine} { // 8 = single word, 11 = sparse text
		for _, cfg := range []probeConfig{
			{label: "gray x3", opts: groupHeaderOptions},
			{label: "gray+thr x2", opts: vision.Options{SkipEqualize: true, SkipInvert: true, UpscaleFactor: 2}},
		} {
			sh := headerShape{
				label: fmt.Sprintf("count-only psm=%d %s", psm, cfg.label),
				rect:  headerCountRegion,
				spec:  ocr.Spec{MinConf: groupHeaderSpec.MinConf, PSM: psm},
				opts:  cfg.opts,
			}
			t.Log("  " + scoreHeaderShape(ctx, t, engine, frames, ranks, totals, sh).String())
		}
	}

	// The character whitelist, through both rectangles. groupHeaderSpec does
	// NOT carry one and this is not a proposal that it should -- CLAUDE.md
	// records a charset forcing tesseract's classifier to reclassify every
	// glyph as a digit and manufacturing "3732407604" out of a crop that was
	// not a number at all, and a count feeds groupTracker.expected, where an
	// under-count stops a group's members being created.
	//
	// It is measured because it is the obvious next thing to reach for once
	// every shape, threshold and PSM has failed on a token of digits, and an
	// argument that it is dangerous is not a measurement of what it does. What
	// it does here is in the numbers: it does not read R2's count either, and
	// it launders R4's "2/9" into a wrong total. A claim that a whitelist was
	// ruled out has to be re-runnable to be worth anything.
	for _, r := range rects {
		for _, cfg := range []probeConfig{
			{label: "gray x3", opts: groupHeaderOptions},
			{label: "gray+thr x2", opts: vision.Options{SkipEqualize: true, SkipInvert: true, UpscaleFactor: 2}},
		} {
			sh := headerShape{
				label: fmt.Sprintf("%s charset 0-9/ %s", r.name, cfg.label),
				rect:  r.rect,
				spec:  ocr.Spec{MinConf: groupHeaderSpec.MinConf, Charset: "0123456789/"},
				opts:  cfg.opts,
			}
			t.Log("  " + scoreHeaderShape(ctx, t, engine, frames, ranks, totals, sh).String())
		}
	}
}

// reportRosterHeaderThreshold sweeps the two knobs the skip-flag grid never
// touches: AdaptiveThreshold's block size and its C.
//
// It exists because the shape grid's answer was uniform in the way this repo
// treats as a finding rather than a result: every one of its configurations
// reads R3's "10/64" and none reads R2's "1/11", through either rectangle.
//
// THE HYPOTHESIS THAT BUILT THIS MODE WAS WRONG, and the mode is what
// disproved it. The reasoning was contrast: R2's header card is green, its "1"
// is drawn in green on it, Grayscale collapses the two, and threshold is the
// only step in the chain that can recover a low-luminance-contrast glyph while
// the grid runs it at exactly one block size and one C. All 40 settings, at
// both rectangles, read R2 as "VN", "VL", "Wu" or "U/L" -- the same wrong
// answers the grid gave, not degraded ones. The glyphs are legible to the eye
// in the plain grayscale crop, so what fails is tesseract's classifier on a
// run of near-identical vertical bars, not the contrast the crop presents.
//
// Do not re-derive the contrast hypothesis from this mode's existence. It is
// kept because a negative measured over 40 settings is worth more than the
// argument that replaced it, and because any future change to
// groupHeaderOptions' ThresholdBlock/ThresholdC has to be read against it --
// not because there is anything left down that path for R2.
func reportRosterHeaderThreshold(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame) {
	t.Helper()
	ranks := rosterFrameRanks()
	totals := rosterGroupTotals(t)

	rects := []struct {
		name string
		rect transport.Rect
	}{
		{"header-to-gutter", groupHeaderRegion},
		{"count-only", headerCountRegion},
	}
	for _, r := range rects {
		t.Logf("  through %s (X1=%.4f X2=%.4f):", r.name, r.rect.X1, r.rect.X2)
		for _, block := range []int{9, 15, 25, 41, 61} {
			for _, c := range []int{2, 5, 10, 20} {
				for _, inv := range []bool{true, false} {
					opts := vision.Options{
						SkipEqualize:   true,
						SkipInvert:     inv,
						ThresholdBlock: block,
						ThresholdC:     c,
						UpscaleFactor:  3,
					}
					label := fmt.Sprintf("%s thr b=%d c=%d%s", r.name, block, c, map[bool]string{true: "", false: "+inv"}[inv])
					t.Log("  " + scoreHeaderShape(ctx, t, engine, frames, ranks, totals, headerShape{
						label: label, rect: r.rect, spec: groupHeaderSpec, opts: opts,
					}).String())
				}
			}
		}
	}
}

// channelImage presents one colour channel of an image as a grayscale image,
// so vision.Preprocess's own Grayscale step becomes a no-op and the rest of
// the chain runs on that channel instead of on luma.
//
// It exists for one measured question. The status column renders two states:
// a grey elapsed time and a green "Online", and lastActiveOptions' own comment
// records every "Online" row reading as garbage ("oo", "ae") under every shape
// tried. Luma is 0.299R + 0.587G + 0.114B, which weights green highest of the
// three -- so green text on a cream card is exactly the case luma flattens,
// while a grey elapsed time is by definition equal in all three channels and
// cannot be hurt by choosing one. That is an argument, and this is how it gets
// measured instead.
type channelImage struct {
	src image.Image
	ch  int // 0 red, 1 green, 2 blue
}

func (c channelImage) ColorModel() color.Model { return color.GrayModel }
func (c channelImage) Bounds() image.Rectangle { return c.src.Bounds() }
func (c channelImage) At(x, y int) color.Color {
	r, g, b, _ := c.src.At(x, y).RGBA()
	v := r
	switch c.ch {
	case 1:
		v = g
	case 2:
		v = b
	}
	return color.Gray{Y: uint8(v >> 8)}
}

// lastActiveShape is one way of preparing the status crop, named for the
// report.
type lastActiveShape struct {
	label string
	// wrap prepares the frame before vision.Preprocess sees it. nil is the
	// shipped path: Preprocess's own luma Grayscale.
	wrap func(image.Image) image.Image
	opts vision.Options
}

// lastActiveCounts is one shape's score over the bands of one truth class.
type lastActiveCounts struct {
	Bands, Parsed, Exact, Empty int
	// Wrong is a read that parsed to a value BELOW the transcribed one.
	// last_active only advances while a capture runs, so a lower value cannot
	// be the clock moving and is a confident wrong number -- the outcome this
	// field's retry has to be measured against, not merely its parse rate.
	Wrong int
}

// combinedLabel names the shipped read with a green-channel retry on a read
// that did not parse.
const combinedLabel = "shipped+green retry"

// reportRosterLastActive measures the status column against the transcribed
// last_active, split by what that value is.
//
// The split is the whole point. The field's two states are drawn differently
// -- a grey elapsed time and a green "Online" -- and a single accuracy over
// both cannot say whether a shape helps the state that is failing. Capture 1's
// roster is roughly half online, so a shape that reads every elapsed time and
// no Online row scores about 50% and looks like a coin flip rather than like a
// colour problem.
//
// Bands are attributed to a member through the NAME read, at AutoAccept, so a
// band whose name cannot be resolved is not scored at all: without a member
// there is no transcribed value to score against. That undercounts, and it
// undercounts the same way for every shape.
//
// `exact` is stricter than `parsed` and neither is the whole answer.
// last_active ADVANCES while a capture runs -- the fixture records the value
// on the earliest frame a member appears on, and its own header says
// Rudester17 reads "1m ago" on seq 22 and "11m ago" on seq 58 -- so a later
// frame reading one step further on is correct and scores as parsed-not-exact.
// For the Online class the two nearly coincide, because a row that was online
// at the start of the capture is almost always still online at the end.
func reportRosterLastActive(ctx context.Context, t *testing.T, engine ocr.OCREngine, frames []rosterLoadedFrame, truth rosterTruth) {
	t.Helper()

	ing := &Ingester{engine: engine}
	green := func(i image.Image) image.Image { return channelImage{i, 1} }
	// The default four are the shipped shape and the three shapes
	// -roster.lasweep put at the top of its grid over the online bands. They
	// are re-measured here over BOTH classes, because the sweep skips the
	// elapsed bands for cost and a shape that helps one state can cost the
	// other.
	shapes := []lastActiveShape{
		{label: "shipped (luma)", opts: lastActiveOptions},
		{label: "luma thr+inv", opts: vision.Options{SkipEqualize: true, UpscaleFactor: 3}},
		{label: "green chan", wrap: green, opts: lastActiveOptions},
		{label: "green chan inv", wrap: green, opts: vision.Options{SkipEqualize: true, SkipThreshold: true, UpscaleFactor: 3}},
	}
	specs := []ocr.Spec{lastActiveSpec}
	if *rosterLASweep {
		shapes = lastActiveSweepShapes()
		specs = []ocr.Spec{
			lastActiveSpec,
			{Charset: lastActiveSpec.Charset, MinConf: lastActiveSpec.MinConf, PSM: 8},
			{Charset: lastActiveSpec.Charset, MinConf: lastActiveSpec.MinConf, PSM: ocr.PSMRawLine},
		}
	}
	type shapeSpec struct {
		sh   lastActiveShape
		spec ocr.Spec
	}
	var runs []shapeSpec
	for _, sh := range shapes {
		for _, sp := range specs {
			label := sh.label
			if len(specs) > 1 {
				psm := sp.PSM
				if psm == 0 {
					psm = ocr.PSMSingleLine
				}
				label = fmt.Sprintf("%s psm%d", sh.label, psm)
			}
			runs = append(runs, shapeSpec{lastActiveShape{label: label, wrap: sh.wrap, opts: sh.opts}, sp})
		}
	}
	shapes = nil
	for _, r := range runs {
		shapes = append(shapes, r.sh)
	}
	specOf := map[string]ocr.Spec{}
	for _, r := range runs {
		specOf[r.sh.label] = r.spec
	}

	// classOf is the transcribed value's class, not the read's: "Online" or
	// an elapsed time. It is what the report is split on.
	classOf := func(v string) string {
		if strings.EqualFold(strings.TrimSpace(v), "Online") {
			return "online"
		}
		return "elapsed"
	}

	byShape := map[string]map[string]*lastActiveCounts{}
	for _, sh := range shapes {
		byShape[sh.label] = map[string]*lastActiveCounts{
			"online": {}, "elapsed": {},
		}
	}
	reportOrder := append([]lastActiveShape{}, shapes...)
	if !*rosterLASweep {
		byShape[combinedLabel] = map[string]*lastActiveCounts{"online": {}, "elapsed": {}}
		reportOrder = append(reportOrder, lastActiveShape{label: combinedLabel})
	}
	var detail []string

	for _, f := range frames {
		bands, err := SegmentRows(f.Img, memberListRegion, memberRowPitch)
		if err != nil {
			continue
		}
		for _, band := range bands {
			nameRes, _, err := ing.readFieldWithRetry(ctx, f.Img,
				fieldRect(band, f.Img, nameXFrac0, nameXFrac1, topRowYFrac0, topRowYFrac1),
				readPlan{spec: nameSpec, opts: nameOptions}, nameRetry)
			if err != nil {
				t.Fatalf("reading name on frame %d band %d: %v", f.Seq, band.Y0, err)
			}
			cands := roster.Rank(strings.TrimSpace(nameRes.Text), truth.Members)
			if len(cands) == 0 || cands[0].Score < roster.AutoAccept {
				continue
			}
			member := cands[0].Name
			want := truth.LastActive[member]
			class := classOf(want)
			if *rosterLASweep && class != "online" {
				// The sweep exists for the green state and costs one OCR
				// call per band per shape per PSM. The elapsed state already
				// reads at 250/253 through the shipped shape; re-measuring it
				// 72 times would multiply the run by ten to confirm what is
				// not in question.
				continue
			}
			wantHours, wantErr := ParseLastActiveHours(want)

			rect := fieldRect(band, f.Img, statusXFrac0, statusXFrac1, statusYFrac0, statusYFrac1)
			line := fmt.Sprintf("    frame %2d y=%4d %-22q want %-8q", f.Seq, band.Y0, member, want)
			perShape := map[string]string{}
			for _, sh := range shapes {
				src := f.Img
				if sh.wrap != nil {
					src = sh.wrap(f.Img)
				}
				res, err := ing.readField(ctx, src, rect, specOf[sh.label], sh.opts)
				if err != nil {
					t.Fatalf("reading last-active on frame %d band %d: %v", f.Seq, band.Y0, err)
				}
				c := byShape[sh.label][class]
				c.Bands++
				got, gerr := ParseLastActiveHours(res.Text)
				switch {
				case strings.TrimSpace(res.Text) == "":
					c.Empty++
				case gerr == nil:
					c.Parsed++
					switch {
					case wantErr != nil:
					case got == wantHours:
						c.Exact++
					case got < wantHours:
						// See lastActiveCounts.Wrong. Counted here as well as
						// on the combined shape: counting it on one column
						// and not the others made the retry look like it had
						// manufactured a value the shipped shape had in fact
						// produced on its own, which is an instrument
						// reporting a difference that is entirely its own.
						c.Wrong++
					}
				}
				perShape[sh.label] = strings.TrimSpace(res.Text)
				line += fmt.Sprintf("  | %s %q", sh.label, strings.TrimSpace(res.Text))
			}
			if c, ok := byShape[combinedLabel]; ok {
				// The retry the name field already ships, applied to this
				// field: the shipped read, and on a read that does not PARSE
				// (not merely one that is empty), the green channel's.
				//
				// It is the only combination in this mode that can be better
				// than both its parts, and it is also the one that can be
				// worse than either: CLAUDE.md's rule is that a retry on a
				// NUMBER can manufacture a plausible value where a retry on a
				// name merely fails to match. `wrong` is that risk, counted
				// -- a read that parsed to a value the transcription
				// contradicts by more than the clock could have moved.
				text := perShape["shipped (luma)"]
				if _, err := ParseLastActiveHours(text); err != nil {
					text = perShape["green chan"]
				}
				cc := c[class]
				cc.Bands++
				got, gerr := ParseLastActiveHours(text)
				switch {
				case strings.TrimSpace(text) == "":
					cc.Empty++
				case gerr == nil:
					cc.Parsed++
					if wantErr == nil && got == wantHours {
						cc.Exact++
					} else if wantErr == nil && got < wantHours {
						// last_active only ever advances during a capture, so
						// a read BELOW the transcribed value cannot be the
						// clock moving.
						cc.Wrong++
					}
				}
				line += fmt.Sprintf("  | %s %q", combinedLabel, text)
			}
			detail = append(detail, line)
		}
	}

	t.Log("  last_active, by what the transcription says the row shows:")
	for _, sh := range reportOrder {
		for _, class := range []string{"online", "elapsed"} {
			c := byShape[sh.label][class]
			t.Logf("    %-22s %-8s bands %3d  parsed %3d  exact %3d  wrong %3d  empty %3d",
				sh.label, class, c.Bands, c.Parsed, c.Exact, c.Wrong, c.Empty)
		}
	}
	if *rosterDetail {
		t.Log("  per band:")
		for _, l := range detail {
			t.Log(l)
		}
	}
}

// lastActiveSweepShapes is the option grid -roster.lasweep runs: every
// skip-flag combination at three upscale factors, through luma and through the
// green channel.
//
// It is the same shape of grid -roster.headeropts runs, and for the same
// reason: the shipped Options for this field were fitted when the field was
// believed to be grey text, and "Online" is not grey text. Options fitted for
// one condition are not evidence about another.
func lastActiveSweepShapes() []lastActiveShape {
	greenCh := func(i image.Image) image.Image { return channelImage{i, 1} }
	var out []lastActiveShape
	for _, wrap := range []struct {
		label string
		fn    func(image.Image) image.Image
	}{{"luma", nil}, {"green", greenCh}} {
		for _, eq := range []bool{true, false} {
			for _, th := range []bool{true, false} {
				for _, inv := range []bool{true, false} {
					for _, up := range []int{2, 3, 4} {
						out = append(out, lastActiveShape{
							label: fmt.Sprintf("%s eq%v th%v inv%v x%d", wrap.label, !eq, !th, !inv, up),
							wrap:  wrap.fn,
							opts: vision.Options{
								SkipEqualize: eq, SkipThreshold: th, SkipInvert: inv,
								UpscaleFactor: up,
							},
						})
					}
				}
			}
		}
	}
	return out
}
