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
	// offsetProbes is how many candidate strip positions are considered.
	offsetProbes = 3
	// offsetMinScore is the NCC below which probe 0's best placement is not
	// believed. Measured on the handset (see the evidence in scroll.go's
	// package doc and docs/superpowers/specs/evidence/m4-scrolloffset-
	// 2026-08-13/): 0.78 sits between the worst real peak (0.827, roster)
	// and the deliberate over-swipe's garbage peak (0.706). It is a
	// backstop with real but modest headroom, NOT the primary defence —
	// within a single frame pair a lattice decoy can outscore the true
	// placement (0.86 against 0.83 was measured), so no absolute floor can
	// separate a right answer from a wrong one on its own. The geometry
	// check and probe agreement below do that work; this constant exists
	// only to catch a placement so poor that nothing else could explain it.
	offsetMinScore = 0.78
	// offsetMinMargin is a weak backstop, not a discriminator. Measured
	// against a real over-swipe negative (no correct answer existed in any
	// probe's window): the worst REAL margin observed (0.043) was lower
	// than the garbage over-swipe's margin (0.049), so a margin threshold
	// cannot tell truth from noise here. It is set low deliberately, as a
	// defence only against a curve so flat it says nothing at all — the
	// rejected "average the probe curves" experiment (see the design doc)
	// produced a margin of 0.0041, which is the kind of number this floor
	// exists to catch.
	offsetMinMargin = 0.03
	// offsetAgreeTol is how many pixels apart two probes' argmax may be and
	// still count as agreeing. Probes agreed within 1px on every real frame
	// pair measured; 4 gives that headroom without being loose enough to
	// call two different rows the same placement.
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
//     probe 0's own limit (the load-bearing check — see below); at least one
//     other admissible probe agrees with it within offsetAgreeTol pixels;
//     probe 0's peak score clears offsetMinScore; and probe 0's margin over
//     the best competing placement clears offsetMinMargin.
//
// The geometry check in (4) is the one doing the real work, not the score or
// margin floors: even probe 0 has a limit, and a candidate sitting right up
// against limit(0) means the true offset may simply be beyond what this
// region can measure at all, clipped to whatever the window's edge could
// still see. Requiring headroom of a full stripH below limit(0) is what
// turns "the swipe travelled further than this region can measure" into a
// detectable condition rather than a plausible-looking wrong number.
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
	regionTop := int(region.Y1 * float64(h))
	regionBot := int(region.Y2 * float64(h))
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
		search := transport.Rect{X1: region.X1, Y1: float64(pr.y) / float64(h), X2: region.X2, Y2: region.Y2}
		res, err := Match(prev, pr.img, search, h)
		if err != nil {
			return 0, fmt.Errorf("vision: matching probe %d: %w", pr.p, err)
		}
		pr.d = int(res.Box.Y1*float64(h)) - pr.y
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

	agreed := false
	var agreeDetail string
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
		diff := pr.d - candidate
		if diff < 0 {
			diff = -diff
		}
		if diff <= offsetAgreeTol {
			agreed = true
			break
		}
		agreeDetail = fmt.Sprintf("probe %d (admissible, limit %dpx) reported %dpx", pr.p, pr.limit, pr.d)
	}
	if !agreed {
		if agreeDetail == "" {
			agreeDetail = "no other probe's window reaches the candidate"
		}
		return 0, fmt.Errorf(
			"vision: no other probe corroborated candidate offset %dpx (%s): %w",
			candidate, agreeDetail, ErrOffsetUncertain)
	}

	if p0.score < offsetMinScore {
		return 0, fmt.Errorf("vision: probe 0's best placement scored %.3f, below %.2f: %w", p0.score, offsetMinScore, ErrOffsetUncertain)
	}

	decoy, err := decoyScore(prev, p0, region, h, regionBot, candidate, stripH)
	if err != nil {
		return 0, fmt.Errorf("vision: measuring probe 0's margin: %w", err)
	}
	margin := p0.score - decoy
	if margin < offsetMinMargin {
		return 0, fmt.Errorf(
			"vision: probe 0's margin %.3f (peak %.3f, best competing placement %.3f) is below %.2f: %w",
			margin, p0.score, decoy, offsetMinMargin, ErrOffsetUncertain)
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
		stripRect := transport.Rect{X1: region.X1, Y1: float64(y) / float64(h), X2: region.X2, Y2: float64(y+stripH) / float64(h)}
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

// decoyScore returns the best NCC score probe 0's strip finds anywhere in its
// own search window at least stripH away from candidate — the competing
// placement offsetMinMargin exists to guard against. It searches the band
// before the candidate and the band after it separately (Match has no way to
// search a window with an excluded middle) and returns the stronger of
// whichever bands are wide enough to hold the strip at all; a candidate
// sitting at the very edge of its window can leave one band, or even both,
// too narrow to search, which is reported as no error and a sentinel score
// below any real NCC value rather than treated as a failure — the absence of
// a competing placement is itself information (a wide-open margin), not a
// defect.
func decoyScore(prev image.Image, p0 *probeStrip, region transport.Rect, h, regionBot, candidate, stripH int) (float64, error) {
	best := -2.0 // below any real NCC score, which lives in [-1, 1]
	found := false

	// Band before: d in [0, candidate-stripH).
	if before := candidate - stripH; before > 0 {
		r := transport.Rect{X1: region.X1, Y1: float64(p0.y) / float64(h), X2: region.X2, Y2: float64(p0.y+before+stripH) / float64(h)}
		res, err := Match(prev, p0.img, r, h)
		if err != nil {
			return 0, fmt.Errorf("searching the band before the candidate: %w", err)
		}
		if res.Score > best {
			best = res.Score
		}
		found = true
	}

	// Band after: d in (candidate+stripH, limit].
	if after := p0.limit - (candidate + stripH); after > 0 {
		low := p0.y + candidate + stripH + 1
		r := transport.Rect{X1: region.X1, Y1: float64(low) / float64(h), X2: region.X2, Y2: float64(regionBot) / float64(h)}
		res, err := Match(prev, p0.img, r, h)
		if err != nil {
			return 0, fmt.Errorf("searching the band after the candidate: %w", err)
		}
		if res.Score > best {
			best = res.Score
		}
		found = true
	}

	if !found {
		// Both bands were too narrow to hold the strip at all — the
		// candidate has nothing to compete against within this window.
		return -2.0, nil
	}
	return best, nil
}
