//go:build m4probe

// The M4 name-matching probe: how many of the gate's 86 members does the name
// field actually resolve, and which ones does it miss?
//
// This exists because the numbers that set today's vsNameOptions -- the
// eight-shapes x three-upscales sweep recorded in vs.go's doc comment -- came
// from a harness that was never committed. Rebuilding it cost a session. So
// it is committed this time, tagged out of `make test` the way
// internal/vision/zz_preproc_probe_test.go is, and run by hand:
//
//	make probe-m4                                    # the chosen setting
//	make probe-m4 PROBE_ARGS='-probe.detail'         # per-member, to localize
//	make probe-m4 PROBE_ARGS='-probe.sweep'          # the full options sweep
//
// It is a measuring instrument, not a gate: it asserts nothing and always
// passes. Reading its output is the point. What it reports, per configuration:
//
//	distinct members   how many of the 86 auto-accept from at least one band
//	bands accepted     how many of the ~142 row bands auto-accept at all
//	empty reads        how many bands OCR returned nothing for
//
// The first number is the one the gate turns on. The other two separate its
// two failure modes: a member missed because every band reads wrong (a crop or
// scoring problem) versus one missed because every band reads empty (a missing
// language pack). Those need opposite fixes, and the headline number cannot
// tell them apart -- the same distinction CLAUDE.md draws about `worst-in`
// being a minimum over frames.
//
// It reads no database. IngestVS's geometric dedupe is deliberately NOT
// applied: every band of every frame is scored, so a member visible in three
// overlapping frames gets three chances and a single bad band is visibly
// different from a systematic miss.
package ingest

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
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
	probeDetail = flag.Bool("probe.detail", false,
		"print a per-member line: best score, the read that produced it, and the verdict")
	probeSweep = flag.Bool("probe.sweep", false,
		"sweep all eight preprocess shapes at upscale 2/3/4 instead of the shipped options")
	probeLangs = flag.String("probe.langs", "",
		"override vsNameSpec.Languages, e.g. 'eng+kor+ara+chi_sim+jpn'; empty uses whatever vsNameSpec ships with")
	probeExpected = flag.String("probe.expected", "",
		"path to the hand-checked capture; defaults to fixtures/m4gate/expected.yaml")
	probeBands = flag.Bool("probe.bands", false,
		"dump every row band: frame, y-extent, height, and the raw read — the view that separates a segmentation fault from an OCR one")
	probePSMs = flag.String("probe.psm", "",
		"comma-separated page-segmentation modes to compare, e.g. '7,13'; empty uses the engine default")
	probeFBSweep = flag.Bool("probe.fbsweep", false,
		"sweep the FALLBACK's preprocessing over the same grid while the primary stays fixed; the fallback's options were inherited from the primary and never fitted for raw-line mode")
	probeFallbackPSM = flag.Int("probe.psmfallback", 0,
		"when the primary PSM returns empty, retry the same crop at this PSM (13 = raw line); 0 disables")
	probeDumpEmpty = flag.String("probe.dumpempty", "",
		"directory to write the crops of bands that read empty: both the raw band and the preprocessed name crop, so the pixels can be looked at rather than reasoned about")
)

// probeRow and probeCapture are the slice of fixtures/m4gate/expected.yaml
// this probe needs. They duplicate part of gate_test.go's expectedCapture on
// purpose: that file is `package ingest_test` and this one must be `package
// ingest` to reach vsNameOptions, vsNameSpec and fieldRect, which are
// unexported. Sharing the type would mean exporting test-only machinery from
// the production package to serve a probe.
type probeCapture struct {
	GameVersion string       `yaml:"game_version"`
	Frames      []probeFrame `yaml:"frames"`
	Rows        []probeRow   `yaml:"rows"`
}

type probeFrame struct {
	Seq    int    `yaml:"seq"`
	SHA256 string `yaml:"sha256"`
	// OffsetPx is how far the list scrolled between this frame and the
	// previous one, as measured at capture time. Only the assignment probe
	// reads it — the name probe deliberately scores every band of every frame
	// with no deduplication at all — but it lives here because this is the
	// struct that mirrors fixtures/m4gate/expected.yaml.
	OffsetPx int `yaml:"offset_px"`
}

type probeRow struct {
	Rank   int    `yaml:"rank"`
	Name   string `yaml:"name"`
	Points int64  `yaml:"points"`
}

// probeResult is one configuration's score.
type probeResult struct {
	Label     string
	Distinct  int // members auto-accepting from at least one band
	Bands     int // bands auto-accepting
	Empty     int // bands OCR returned nothing for
	TotalBand int

	// Best is each member's best-scoring read across every band, keyed by the
	// hand-transcribed name. This is what -probe.detail prints and what
	// separates "read wrong" from "read empty".
	Best map[string]bandScore
}

// bandScore is one member's best showing across every band: the score, the
// read that produced it, and — when some other member outscored them on that
// same read — who won it and at what. That last pair is the false-attribution
// risk made visible: a member losing their own row to a rival at a comparable
// score is the failure the AutoAccept threshold exists to prevent, and any
// change to scoring has to be read against it rather than against the
// headline count alone.
type bandScore struct {
	Score int
	Text  string
	Frame int
	WonBy string
	WonAt int
}

func TestM4NameProbe(t *testing.T) {
	ctx := context.Background()

	exp := loadProbeCapture(t)
	engine := ocr.NewTesseractEngine()
	if !engine.Available() {
		t.Skip("tesseract is not on PATH; see the gate's skip message")
	}

	frames := loadProbeFrames(t, ctx, exp)
	t.Logf("probe: %d frames, game version %s, %d hand-transcribed members",
		len(frames), exp.GameVersion, len(exp.Rows))

	members := probeMembers(exp)

	// The safety half of the measurement, printed before any accuracy number
	// so it is not read as an afterthought. Confusable-aware scoring buys
	// matches by making some substitutions cheap, and the bill for that is
	// paid in separation between real members. If the closest pair of
	// genuinely different names on this roster ever reaches AutoAccept, the
	// matcher can attribute one member's row to another — the one failure
	// here that a human cannot undo later.
	//
	// Built from `members` (probeMembers(exp)), not straight off exp.Rows, so
	// this widens automatically the day a fixture carries a broader roster --
	// see ClosestPairScore's doc comment for why the ranked rows are the wrong
	// set in general. It does NOT widen anything today: probeMembers builds
	// its list 1:1 from exp.Rows, because this probe has no roster to draw on
	// beyond the hand-transcribed capture, so `names` below is still exactly
	// the 86 ranked names. The real widening -- checking members who never
	// scored -- only happens in production IngestVS, which has the full
	// alliance roster to check against.
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, m.Name)
	}
	closest, a, b := roster.ClosestPairScore(names)
	t.Logf("closest pair among the %d ranked rows (probe has no broader roster to check): %d (%q vs %q); AutoAccept is %d, margin %d",
		len(names), closest, a, b, roster.AutoAccept, roster.AutoAccept-closest)
	if closest >= roster.AutoAccept {
		t.Errorf("two distinct members score %d against each other — at or above AutoAccept (%d). "+
			"Scoring is too permissive: raise confusableCost, or record these two as needing an alias.",
			closest, roster.AutoAccept)
	}

	for _, psm := range probePSMList(t, engine.PSM) {
		engine.PSM = psm
		for _, cfg := range probeConfigs() {
			for _, fb := range fallbackConfigs() {
				// Both plans are the shipped ones by default, so a bare
				// `make probe-m4` measures exactly what IngestVS does. The
				// flags override one dimension at a time.
				primary := readPlan{spec: vsNameSpec, opts: cfg.opts}
				retry := readPlan{spec: vsNameRetry.spec, opts: fb.opts}
				if *probeLangs != "" {
					primary.spec.Languages = *probeLangs
					retry.spec.Languages = *probeLangs
				}
				if *probeFallbackPSM != 0 {
					retry.spec.PSM = *probeFallbackPSM
				}
				label := fmt.Sprintf("psm%d %s", psm, cfg.label)
				if *probeFBSweep {
					label = fmt.Sprintf("fb:%-13s", fb.label)
				}
				res := scoreConfig(ctx, t, engine, frames, members, exp, primary, retry, label)
				t.Logf("%-22s  %3d/%d distinct  %3d/%d bands  %2d empty",
					res.Label, res.Distinct, len(exp.Rows), res.Bands, res.TotalBand, res.Empty)

				if *probeDetail {
					printDetail(t, exp, res)
				}
			}
		}
	}
}

// fallbackConfigs is what the retry preprocesses with: the shipped
// vsNameRetry options unless -probe.fbsweep asks for the whole grid. The sweep
// exists because the retry originally inherited the primary's options, and
// that inheritance was an assumption rather than a result — the primary's
// options were fitted for a mode whose layout analysis works, and the retry
// runs only on crops where it does not. Sweeping it was worth two members.
func fallbackConfigs() []probeConfig {
	if !*probeFBSweep {
		return []probeConfig{{label: "shipped-retry", opts: vsNameRetry.opts}}
	}
	return probeShapeGrid()
}

// probePSMList parses -probe.psm, defaulting to whatever the engine ships
// with. Comparing modes is a first-class thing to want here: the shipped PSM 7
// returns the empty string for a legible, correctly-framed crop, and which
// modes do not is a measurement over all 142 bands rather than over the
// handful anyone looks at by hand.
func probePSMList(t *testing.T, def int) []int {
	t.Helper()
	if *probePSMs == "" {
		return []int{def}
	}
	return parsePSMList(t, *probePSMs)
}

type probeConfig struct {
	label string
	opts  vision.Options
}

// probeConfigs is the shipped options unless -probe.sweep asks for the full
// grid. The grid is the same one that set vsNameOptions: every combination of
// the three skip flags at upscale 2, 3 and 4.
func probeConfigs() []probeConfig {
	if !*probeSweep {
		return []probeConfig{{label: "shipped", opts: vsNameOptions}}
	}
	return probeShapeGrid()
}

// probeShapeGrid is every combination of the three skip flags at upscale 2, 3
// and 4 — the grid that set vsNameOptions, shared by the primary and fallback
// sweeps so the two are directly comparable.
func probeShapeGrid() []probeConfig {
	var out []probeConfig
	for _, up := range []int{2, 3, 4} {
		for _, shape := range []struct {
			name         string
			eq, thr, inv bool
		}{
			{"full", false, false, false},
			{"gray", true, true, true},
			{"gray+eq", false, true, true},
			{"gray+thr", true, false, true},
			{"gray+inv", true, true, false},
			{"gray+eq+thr", false, false, true},
			{"gray+eq+inv", false, true, false},
			{"gray+thr+inv", true, false, false},
		} {
			out = append(out, probeConfig{
				label: fmt.Sprintf("%s x%d", shape.name, up),
				opts: vision.Options{
					SkipEqualize:  shape.eq,
					SkipThreshold: shape.thr,
					SkipInvert:    shape.inv,
					UpscaleFactor: up,
				},
			})
		}
	}
	return out
}

// scoreConfig reads every band of every frame under one configuration and
// scores the reads against the hand-transcribed names.
func scoreConfig(ctx context.Context, t *testing.T, engine *ocr.TesseractEngine, frames []probeLoadedFrame, members []roster.Member, exp probeCapture, primary, retry readPlan, label string) probeResult {
	t.Helper()

	res := probeResult{Label: label, Best: map[string]bandScore{}}
	// nameByID indexes back from the synthetic member id to the transcribed
	// name, so a match can be attributed to the row a human read.
	nameByID := make(map[int64]string, len(members))
	for _, m := range members {
		nameByID[m.ID] = m.Name
	}

	ing := &Ingester{engine: engine}
	for _, f := range frames {
		bands, err := SegmentRows(f.Img, vsListRegion, vsRowPitch)
		if err != nil {
			t.Fatalf("segmenting frame %d: %v", f.Seq, err)
		}
		for _, band := range bands {
			res.TotalBand++
			rect := fieldRect(band, f.Img, vsNameXFrac0, vsNameXFrac1, vsNameYFrac0, vsNameYFrac1)
			// The production read path, retry and all, so the probe cannot
			// drift from what IngestVS does.
			read, _, err := ing.readFieldWithRetry(ctx, f.Img, rect, primary, retry)
			if err != nil {
				t.Fatalf("reading frame %d band %d: %v", f.Seq, band.Y0, err)
			}
			text := strings.TrimSpace(read.Text)

			if *probeBands {
				// Height and y-extent are logged alongside the read because an
				// empty read has two very different causes that only these
				// numbers tell apart: a band that is the wrong shape (clipped
				// at a region edge, or a half-row at the seam between frames)
				// versus a correctly-shaped band the engine could not read.
				t.Logf("  band frame=%2d y=%4d..%4d h=%3d  crop=%.3f..%.3f  read=%q",
					f.Seq, band.Y0, band.Y1, band.Height(), rect.Y1, rect.Y2, text)
			}
			if text == "" {
				res.Empty++
				if *probeDumpEmpty != "" {
					dumpEmptyBand(t, *probeDumpEmpty, f, band, rect, primary.opts)
				}
				continue
			}

			cands := roster.Rank(text, members)
			if len(cands) == 0 {
				continue
			}
			if cands[0].Score >= roster.AutoAccept {
				res.Bands++
			}

			// Every member's score is recorded, not just the winner's. Keeping
			// only the top candidate conflates two very different failures: a
			// member nothing was ever read for, and a member read at 88 on a
			// band some other member won at 91. Both would show as zero, and
			// they need opposite fixes -- which is the exact mistake this
			// probe exists to stop anyone making from an aggregate.
			//
			// Rank already scores every member, so this costs nothing beyond
			// the loop.
			for _, c := range cands {
				name := nameByID[c.MemberID]
				if prev, ok := res.Best[name]; !ok || c.Score > prev.Score {
					res.Best[name] = bandScore{Score: c.Score, Text: text, Frame: f.Seq, WonBy: nameByID[cands[0].MemberID], WonAt: cands[0].Score}
				}
			}
		}
	}

	for _, b := range res.Best {
		if b.Score >= roster.AutoAccept {
			res.Distinct++
		}
	}
	return res
}

// dumpEmptyBand writes two PNGs for one band that read empty: the whole band
// as captured, and the preprocessed name crop exactly as the engine received
// it. Two images rather than one because they answer different questions —
// the band says whether a row is there at all and where its name line sits,
// and the crop says what the engine was actually handed. An empty read with a
// legible crop and an empty read with a blank crop are different bugs.
func dumpEmptyBand(t *testing.T, dir string, f probeLoadedFrame, band RowBand, rect transport.Rect, opts vision.Options) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating dump dir: %v", err)
	}

	b := f.Img.Bounds()
	full := image.Rect(b.Min.X, band.Y0, b.Max.X, band.Y1)
	writePNG(t, filepath.Join(dir, fmt.Sprintf("f%02d-y%04d-band.png", f.Seq, band.Y0)),
		f.Img.(interface {
			SubImage(image.Rectangle) image.Image
		}).SubImage(full))

	o := opts
	o.Region = rect
	writePNG(t, filepath.Join(dir, fmt.Sprintf("f%02d-y%04d-crop.png", f.Seq, band.Y0)), visionPreprocess(f.Img, o))
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	fh, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	defer fh.Close()
	if err := png.Encode(fh, img); err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
}

// printDetail lists every member the configuration did not auto-accept, worst
// first, with the read that came closest. This is the localizing view: an
// EMPTY verdict is a language-pack problem and a REVIEW/MISS verdict with a
// near-miss read is a scoring problem, and they need opposite fixes.
func printDetail(t *testing.T, exp probeCapture, res probeResult) {
	t.Helper()

	type line struct {
		rank int
		name string
		best bandScore
	}
	var missed []line
	for _, row := range exp.Rows {
		b := res.Best[row.Name]
		if b.Score >= roster.AutoAccept {
			continue
		}
		missed = append(missed, line{rank: row.Rank, name: row.Name, best: b})
	}
	sort.Slice(missed, func(i, j int) bool { return missed[i].best.Score < missed[j].best.Score })

	t.Logf("--- %s: %d members not auto-accepted ---", res.Label, len(missed))
	for _, m := range missed {
		// NOTHING means no read anywhere shared enough with this name to score
		// at all — look at the bands, not at the matcher. LOST means the best
		// read WAS this member's row but another member outscored them on it,
		// which is a scoring problem and a false-attribution risk in one.
		verdict := "MISS"
		switch {
		case m.best.Score == 0:
			verdict = "NOTHING"
		case m.best.WonBy != m.name && m.best.WonAt >= roster.AutoAccept:
			verdict = "LOST"
		case m.best.Score >= roster.ReviewFloor:
			verdict = "REVIEW"
		}
		lost := ""
		if verdict == "LOST" {
			lost = fmt.Sprintf("  <- won by %q at %d", m.best.WonBy, m.best.WonAt)
		}
		t.Logf("  rank %3d  %-24q score %3d  %-7s best read %q (frame %d)%s",
			m.rank, m.name, m.best.Score, verdict, m.best.Text, m.best.Frame, lost)
	}
}

// probeMembers builds the matcher's member list from the hand-transcribed
// names, with no aliases. That is the cold-run case on purpose: the gate
// measures a first capture against a roster nobody has resolved a review for
// yet, which is the hardest case the pipeline ever faces.
func probeMembers(exp probeCapture) []roster.Member {
	out := make([]roster.Member, 0, len(exp.Rows))
	for i, row := range exp.Rows {
		out = append(out, roster.Member{ID: int64(i + 1), Name: row.Name})
	}
	return out
}

type probeLoadedFrame struct {
	Seq int
	Img image.Image
}

func loadProbeFrames(t *testing.T, ctx context.Context, exp probeCapture) []probeLoadedFrame {
	t.Helper()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load(): %v", err)
	}
	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		t.Fatalf("opening blob store (%s): %v", cfg.Blob.Backend, err)
	}

	sort.Slice(exp.Frames, func(i, j int) bool { return exp.Frames[i].Seq < exp.Frames[j].Seq })
	out := make([]probeLoadedFrame, 0, len(exp.Frames))
	for _, f := range exp.Frames {
		rc, err := blobs.Get(ctx, blob.Key(f.SHA256))
		if err != nil {
			// Skip rather than fail, matching the gate: an absent capture means
			// the store is not pointed where the frames were written, which is
			// a setup problem and not a measurement.
			t.Skipf("frame %d (%s) is not in the %s blob store; set LW_BLOB_FS_ROOT to an absolute path (see CLAUDE.md)",
				f.Seq, f.SHA256, cfg.Blob.Backend)
		}
		img, err := png.Decode(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("decoding frame %d: %v", f.Seq, err)
		}
		out = append(out, probeLoadedFrame{Seq: f.Seq, Img: img})
	}
	return out
}

func loadProbeCapture(t *testing.T) probeCapture {
	t.Helper()

	path := *probeExpected
	if path == "" {
		path = filepath.Join("..", "..", "fixtures", "m4gate", "expected.yaml")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("no hand-checked capture at %s", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var exp probeCapture
	if err := yaml.Unmarshal(data, &exp); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if len(exp.Rows) == 0 || len(exp.Frames) == 0 {
		t.Fatalf("%s: needs both frames and rows", path)
	}
	return exp
}
