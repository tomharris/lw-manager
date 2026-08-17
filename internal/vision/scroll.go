package vision

import (
	"errors"
	"fmt"
	"image"
	"math"
	"strings"

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
	// offsetProbes is how many candidate strip positions are considered.
	offsetProbes = 3
	// offsetMinScore is the NCC below which probe 0's best placement is not
	// believed — and, as of task 26, is deliberately no longer trying to be
	// anything sharper than that.
	//
	// It was originally set at 0.75, calibrated on device run 365's 16 real
	// roster pairs (worst real peak 0.799) sitting close to the midpoint
	// against the roster's own over-swipe garbage peak (0.706), and then
	// applied to VS without ever being checked against VS — which is why
	// `vs_capture` could not complete a run: two live handset runs each died
	// here, "run 368 frame 4: ... scored 0.641, below 0.75" and "run 369
	// frame 1: ... scored 0.748, below 0.75". Both messages name the score
	// check specifically, meaning reach and agreement had already passed —
	// these were real, agreed-upon placements, not garbage that slipped past
	// the wrong criterion.
	//
	// Measuring the full VS distribution (docs/superpowers/specs/evidence/
	// m4-scrolloffset-2026-08-13/, plus device runs 368/369's own error
	// text) found six real VS observations ranging 0.641-0.884 — genuinely
	// lower than the roster's 0.799-0.907, not a fluke of two bad frames.
	// And VS's own over-swipe garbage peak (0.702, rejected by the reach
	// check before score is ever consulted — see ScrollOffset's doc comment)
	// sits *above* VS's own real minimum (0.641). That is the same overlap
	// that got offsetMinMargin deleted rather than tuned: a threshold with
	// real values on both sides of it discriminates nothing, at any value.
	// No single number can separate "correct" from "garbage" on VS, and
	// unlike the margin check there is no cliff-edge fix available here
	// either — VS's real range and its garbage range are not merely close,
	// they overlap outright.
	//
	// So this constant has stopped doing discrimination work and started
	// doing only backstop work: 0.5 is comfortably below every real
	// observation measured on either screen (0.641 the tightest, ~0.14 of
	// margin) while still refusing a placement so poor nothing else could
	// explain it — near-zero or negative correlation, the signature of
	// comparing genuinely unrelated content rather than a slightly weak
	// match on the right one. The geometry check and probe agreement below
	// are what actually do the discriminating, on both screens; this
	// constant no longer claims to.
	offsetMinScore = 0.5
	// offsetAgreeTol is how many pixels apart two probes' argmax may be and
	// still count as agreeing. Across the same 16 real pairs the three
	// probes' argmax spread by 0-2px in every pair — probes 0 and 1 were
	// identical in every single pair, and probe 2 was consistently 1px
	// lower, a systematic sub-pixel effect rather than noise — against a
	// 255px spread (757/629/502) on the deliberate over-swipe negative. 4
	// keeps headroom over the real spread without being loose enough to
	// call two different rows the same placement.
	//
	// There is deliberately no margin floor here. One was tried
	// (offsetMinMargin = 0.03, comparing probe 0's peak against its best
	// competing placement at least one strip away) and deleted after the
	// same 16-pair run: real margins ran 0.0459-0.1133, the one pair that
	// still failed on hardware margined at 0.021, and the over-swipe
	// negative's margin was 0.049 — inside the real range, not below it. No
	// threshold t can satisfy "0.021 passes" and "0.049 fails"
	// simultaneously, so a margin floor has false positives (rejects a real
	// capture) and zero true positives (does not reject the case it was
	// added for) at every value. Deleted rather than loosened: a floor that
	// cannot separate truth from noise is worse than no floor, because it
	// manufactures a confident-looking failure for a non-reason. Agreement
	// above is what actually discriminates real pairs from garbage — 0-2px
	// against 255px is a real gap; no margin value ever produced one.
	offsetAgreeTol = 4
	// offsetMinVariance rejects a probe strip flat enough to correlate with
	// anything, which is the same trap that makes a near-flat anchor
	// useless: NCC divides out the template's variance, so a flat strip
	// asks "is this area smooth" and every gap between cards answers yes.
	// This is PER PIXEL — real strips measured 1.6e8-2.3e8 for a 677x90
	// crop (~2,600-3,800 per pixel), so an un-normalized sum-based floor of
	// 50 (this constant's previous value) could never fire; 100 per pixel
	// (stddev 10) clears real content by more than an order of magnitude
	// while still catching a blank crop.
	offsetMinVariance = 100.0
)

// ScrollOffset measures how far the content inside region moved up between
// prev and cur, in pixels of the frames' own resolution. Zero means the
// content did not move.
//
// It works by cutting probe strips from cur at successive depths into the
// region and finding where each sits in prev. Content moving up by d puts a
// feature that was at y+d in prev at y in cur, so every strip is searched
// downward only, bounded by the region's bottom — which means a probe placed
// deeper into the region can measure less travel than one placed at the top:
// probe p's search window can prove at most
//
//	limit(p) = regionBot - stripY(p) - stripH
//
// pixels of movement. A probe whose limit falls short of the true travel does
// not fail loudly — the true placement is simply outside its search window,
// so it returns the best thing that IS present. For a scrolling list of
// uniformly-pitched rows, that is a lattice decoy: the strongest wrong
// answers sit at whole multiples of the row pitch away from the truth,
// because a repeating list correlates with itself once per row. Recon on the
// handset caught this exactly: probe 2 (the deepest, and — under the old
// scheme this replaced — the one bestProbeStrip preferred by variance alone)
// reported a travel of 282px with no error, on a frame pair whose true travel
// was 665px; the 383px difference is almost exactly three row pitches.
//
// The fix is not a tighter threshold — recon proved no absolute score floor
// separates a right answer from a wrong one within a single pair (a lattice
// decoy scored 0.86 against a true peak of 0.83 on the same frames). Instead:
//
//  1. Every probe is cut, and any whose strip is too flat to mean anything
//     (per-pixel variance below offsetMinVariance) is rejected outright.
//  2. Probe 0 — the shallowest, and so the one with the largest reach — is
//     the sole source of the candidate offset. It is the only probe that is
//     never blind before every other probe already is, so nothing downstream
//     needs to guess which probe to trust: there is only ever one candidate.
//  3. A surviving probe VOTES for that candidate only if it is admissible for
//     it, meaning limit(p) >= candidate — the arithmetic condition for the
//     candidate to actually lie inside that probe's own search window, i.e.
//     for the probe to be capable of having found it at all. An inadmissible
//     probe is discarded entirely: it never averages in, never votes, and
//     never breaks a tie, because a probe that could not have seen the
//     candidate has nothing to say about whether it is correct. (Averaging
//     every probe's curve regardless of reach was tried and measured: it
//     improved the roster's margin but returned d=538 at a margin of 0.0041
//     on the VS pair above — an answer neither honest probe produced —
//     because it let the blind probe outvote the sighted ones.)
//  4. The candidate is accepted only if all of: it does not press against
//     probe 0's own limit (the load-bearing check — see below); EVERY other
//     admissible probe agrees with it within offsetAgreeTol pixels, not just
//     one of them; and probe 0's peak score clears offsetMinScore.
//
// The geometry check and "every admissible probe agrees" are what actually do
// the work, not the score floor: even probe 0 has a limit, and a candidate
// sitting right up against limit(0) means the true offset may simply be
// beyond what this region can measure at all, clipped to whatever the
// window's edge could still see. Requiring headroom of a full stripH below
// limit(0) is what turns "the swipe travelled further than this region can
// measure" into a detectable condition rather than a plausible-looking wrong
// number. Agreement is what separates real pairs from garbage in practice —
// device run 365 measured a 0-2px argmax spread across 16 real frame pairs
// against a 255px spread on a deliberate over-swipe negative — which is why
// it now takes only one disagreeing admissible probe to refuse, not a
// majority. There is deliberately no margin check between probe 0's peak and
// its best competing placement; see offsetMinScore's own doc comment for the
// arithmetic on why one was tried and deleted rather than tuned.
//
// The return value is a raw pixel count across the frames' native resolution,
// not a normalized coordinate. This is safe because a scroll delta is a
// distance measured within one image and consumed only alongside those same
// frames — it never addresses a screen location (which is where the
// normalized-coordinates invariant applies). The tradeoff: a persisted offset
// stops being self-describing if a device with different resolution joins the
// fleet, so offsets are meaningful only in the context of their source frames.
func ScrollOffset(prev, cur image.Image, region transport.Rect) (int, error) {
	if !region.Valid() {
		return 0, fmt.Errorf("vision: scroll region %+v is not a valid unit-square rect", region)
	}
	if prev.Bounds() != cur.Bounds() {
		return 0, fmt.Errorf("vision: frames differ in size: %v vs %v", prev.Bounds(), cur.Bounds())
	}

	h := cur.Bounds().Dy()
	// math.Round, not truncation: Match rounds a region's fractional bounds
	// the same way (matcher.go), and truncation here instead rounds
	// regionBot down, which can sit up to 1px below what Match's own search
	// window actually admits — understating every limit(p) by that same
	// 1px. Conservative either way (a tighter limit only ever rejects a
	// borderline-valid candidate, never accepts a bad one), but consistency
	// with Match's own rule removes the question rather than leaving it as
	// a note.
	regionTop := int(math.Round(region.Y1 * float64(h)))
	regionBot := int(math.Round(region.Y2 * float64(h)))
	regionH := regionBot - regionTop
	stripH := int(offsetStripFrac * float64(regionH))
	if stripH < 8 || regionH < 4*stripH {
		return 0, fmt.Errorf("vision: region is too short to measure a scroll offset (%d px): %w", regionH, ErrOffsetUncertain)
	}

	probes, err := cutProbes(cur, region, regionTop, regionBot, h, stripH)
	if err != nil {
		return 0, err
	}

	// Match every surviving probe over its own search window: from the
	// probe's own row down to the region's bottom. Anything above stripY(p)
	// would mean the list scrolled backwards, which this capture loop never
	// does.
	for i := range probes {
		pr := &probes[i]
		search := transport.Rect{X1: region.X1, Y1: pxFrac(pr.y, h), X2: region.X2, Y2: region.Y2}
		res, err := Match(prev, pr.img, search, h)
		if err != nil {
			return 0, fmt.Errorf("vision: matching probe %d: %w", pr.p, err)
		}
		// math.Round, not truncation: Box.Y1 is itself the ratio of a
		// rounded pixel row (matcher.go rounds the same way when placing a
		// match), so truncating the round-trip back to pixels can read 1px
		// short at exactly the rows where that rounding pushed up rather
		// than down — harmless in practice (within offsetAgreeTol, and
		// ingest's dedupe tolerates pitch/2) but this is the one function
		// whose whole job is an exact number.
		pr.d = int(math.Round(res.Box.Y1*float64(h))) - pr.y
		if pr.d < 0 {
			pr.d = 0
		}
		pr.score = res.Score
	}

	p0 := probeByIndex(probes, 0)
	if p0 == nil {
		return 0, fmt.Errorf("vision: probe 0 was rejected as too flat to source a candidate offset: %w", ErrOffsetUncertain)
	}
	candidate := p0.d

	// probe 0 has the largest reach of any probe, so if the candidate is
	// pressed up against probe 0's own edge, the true offset may simply lie
	// past it, clipped to whatever probe 0's window could still see. This is
	// the load-bearing check: see the doc comment above for why score and
	// margin cannot do this job alone.
	if candidate > p0.limit-stripH {
		return 0, fmt.Errorf(
			"vision: candidate offset %dpx is within one probe strip of probe 0's own reach limit %dpx — the swipe may have travelled further than this region can measure: %w",
			candidate, p0.limit, ErrOffsetUncertain)
	}

	// EVERY admissible probe (every other probe whose own window reaches the
	// candidate at all) must agree, not just one of them — a single
	// admissible probe that disagrees is a refusal. This is deliberately
	// stronger than "any admissible probe agrees": device run 365 measured
	// real pairs agreeing to within 0-2px across all three probes every
	// time, so requiring unanimity costs nothing on real data and catches a
	// probe that is admissible on paper (its window reaches the candidate)
	// but landed on a different placement in practice.
	otherAdmissible := 0
	var disagreements []string
	for _, pr := range probes {
		if pr.p == 0 {
			continue
		}
		if pr.limit < candidate {
			// Inadmissible: this probe's own window does not reach the
			// candidate, so it cannot corroborate or contradict it. It is
			// discarded entirely, not counted as disagreement.
			continue
		}
		otherAdmissible++
		diff := pr.d - candidate
		if diff < 0 {
			diff = -diff
		}
		if diff > offsetAgreeTol {
			disagreements = append(disagreements, fmt.Sprintf("probe %d (limit %dpx) reported %dpx", pr.p, pr.limit, pr.d))
		}
	}
	if otherAdmissible == 0 {
		return 0, fmt.Errorf(
			"vision: no other probe's window reaches candidate offset %dpx to corroborate it: %w",
			candidate, ErrOffsetUncertain)
	}
	if len(disagreements) > 0 {
		return 0, fmt.Errorf(
			"vision: %d of %d admissible probes disagreed with candidate offset %dpx (%s): %w",
			len(disagreements), otherAdmissible, candidate, strings.Join(disagreements, "; "), ErrOffsetUncertain)
	}

	if p0.score < offsetMinScore {
		return 0, fmt.Errorf("vision: probe 0's best placement scored %.3f, below %.2f: %w", p0.score, offsetMinScore, ErrOffsetUncertain)
	}

	return candidate, nil
}

// probeStrip is one candidate probe strip cut from cur, plus what matching it
// against prev found.
type probeStrip struct {
	p     int // probe index, 0 = shallowest = largest reach
	y     int // this probe's row in cur
	limit int // furthest travel this probe's own search window can prove
	img   image.Image
	d     int     // argmax: the offset this probe's best placement was found at
	score float64 // that placement's NCC score
}

// pxFrac converts a pixel row back to the [0,1] fraction a transport.Rect
// needs, for a Rect meant to land on that exact row. float64(px)/float64(h)
// alone is not safe for that: h values this package actually sees (1600 for
// the real VS/roster geometry among them) are not powers of two, so the
// division can round down by one ULP and the multiplication back inside
// Crop (which truncates, unlike Match's own math.Round) recovers px-1
// instead of px — confirmed directly: 414.0/1600.0*1600.0 evaluates to
// 413.999999999999943 in float64, and int() of that is 413. Match's
// rounding absorbs the same error harmlessly, but Crop's truncation does
// not, so a probe strip could be cut starting one row above where cutProbes
// asked for it. The epsilon nudge is many orders of magnitude smaller than
// one pixel at any resolution this package handles and only ever pushes the
// value in the direction that recovers the intended integer.
func pxFrac(px, h int) float64 {
	return (float64(px) + 1e-6) / float64(h)
}

// probeByIndex finds the surviving probe with original index p, or nil if it
// was rejected for flatness. Probes are looked up by their original index
// rather than by position in the slice because rejection can remove any
// probe, including probe 0 itself.
func probeByIndex(probes []probeStrip, p int) *probeStrip {
	for i := range probes {
		if probes[i].p == p {
			return &probes[i]
		}
	}
	return nil
}

// cutProbes cuts every probe strip from cur, placed from the region's own top
// (probe 0 at stripY = regionTop, probe 1 one stripH below it, and so on —
// note p, not p+1: the old scheme started one stripH below the top and so
// gave up an entire strip's worth of probe 0's reach for nothing), and
// rejects any strip too flat to correlate meaningfully against anything (the
// same near-degenerate-template trap documented on offsetMinVariance above).
// Returns an error only when nothing survives — a single flat probe among
// otherwise-good ones is fine, since the accept criteria in ScrollOffset
// already discard an inadmissible or disagreeing probe on their own.
func cutProbes(cur image.Image, region transport.Rect, regionTop, regionBot, h, stripH int) ([]probeStrip, error) {
	var out []probeStrip
	var worst float64
	haveWorst := false
	for p := 0; p < offsetProbes; p++ {
		y := regionTop + p*stripH
		limit := regionBot - y - stripH
		if limit < 0 {
			// This probe's own strip does not fit inside the region at all;
			// offsetProbes*stripH exceeding regionH would have already
			// tripped the regionH < 4*stripH check above for any p this
			// loop reaches, so this is unreachable in practice, but a
			// probe that cannot even be cut is certainly one that cannot
			// be matched.
			continue
		}
		stripRect := transport.Rect{X1: region.X1, Y1: pxFrac(y, h), X2: region.X2, Y2: pxFrac(y+stripH, h)}
		sub := Crop(cur, stripRect)
		perPixel := variance(sub) / float64(sub.Bounds().Dx()*sub.Bounds().Dy())
		if !haveWorst || perPixel < worst {
			worst = perPixel
			haveWorst = true
		}
		if perPixel < offsetMinVariance {
			continue
		}
		out = append(out, probeStrip{p: p, y: y, limit: limit, img: sub})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("vision: every probe strip is too flat to measure (worst per-pixel variance %.1f, floor %.1f): %w", worst, offsetMinVariance, ErrOffsetUncertain)
	}
	return out, nil
}
