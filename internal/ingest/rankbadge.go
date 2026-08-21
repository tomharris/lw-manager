package ingest

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"image"
	"image/png"
	"sort"
	"sync"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrNoConfidentRank reports that no rank-badge template won by a wide
// enough margin to be trusted. It is a sentinel, like ErrUnparseable, so
// callers route the frame to review rather than attributing its rows to a
// guessed rank group.
var ErrNoConfidentRank = errors.New("ingest: rank badge did not match any template with confidence")

//go:embed rankbadges/*.png
var rankBadgeFS embed.FS

// rankBadgeRefHeight is the frame height the badge templates were cut at
// (evidence frame 06, docs/superpowers/specs/evidence/m4-ocr-2026-08-14/
// 06-all-four-rank-groups-collapsed.png — 720x1600, the handset's native
// capture resolution) — vision.Match's refHeight, the same role
// reference_height plays in templates/manifest.yaml for the runtime's own
// anchors.
const rankBadgeRefHeight = 1600

// rankBadgeRegion bounds the search to the sticky header's own badge, not
// the whole groupHeaderRegion band it sits inside: the group name and count
// live in the same OCR'd row but well to the right (x > 0.14 of a 720px
// frame), and excluding them narrows what vision.Match has to search without
// costing any accuracy (a search region only affects how many placements are
// tried, never whether the true one is found — see matcher.go).
//
// Y1/Y2 are groupHeaderRegion's own measured band plus a margin, and they are
// derived from it rather than restated as literals: the badge and the header
// text it sits beside are the same sticky element, so the header band's own
// vertical bounds already contain it, and a version of this that hard-coded
// 0.404..0.438 had already drifted once — those numbers were groupHeaderRegion's
// pre-tightening bounds and stayed put when it moved to 0.409..0.435. Deriving
// makes the next move propagate instead of silently decoupling.
//
// The margin is deliberate and not a fudge: groupHeaderRegion was tightened
// around the *text* for OCR's benefit (see its doc comment), while NCC needs
// room around the badge for the shield outline and shadow that the digit-only
// templates below exclude. A search region only affects how many placements
// are tried, never whether the true one is found (matcher.go), so erring wide
// here costs time rather than accuracy.
//
// X1/X2 (x=30..90 of a 720px frame) were measured directly off evidence frame
// 06 — see docs/superpowers/specs/evidence/m4-ocr-2026-08-14 Finding 4a for
// the pixel-level detail — with the same margin rationale on both sides.
const rankBadgeBandMargin = 8.0 / rankBadgeRefHeight

var rankBadgeRegion = transport.Rect{
	X1: 30.0 / 720,
	Y1: groupHeaderRegion.Y1 - rankBadgeBandMargin,
	X2: 90.0 / 720,
	Y2: groupHeaderRegion.Y2 + rankBadgeBandMargin,
}

// rankBadgeMinGap is the minimum an argmax rank template must lead its
// runner-up by to be trusted — never an absolute score floor. Finding 4a
// measured that a correct match does not land anywhere near 1.0 once the
// probe comes from a different capture than the templates: cross-capture
// (templates cut from evidence frame 06, probed against capture 1's own real
// sticky headers, with vision.Match's own sliding search rather than a
// pixel-exact compare) put the true rank first every time, but at gaps of
// 0.589 (R4), 0.250 (R3) and 0.264 (R2) over the runner-up — nowhere near
// the 1.000 a same-frame self-match produces, and nowhere near each other
// either. No absolute threshold could accept 0.250 and still mean anything
// as a floor. What the floor must do instead is sit below every real gap while
// still rejecting a badge crop that matches nothing in particular, where every
// template scores similarly low and the gap collapses toward zero — the same
// shape of acceptance rule ScrollOffset's probe agreement and phase-locking's
// contrast both landed on, for the same reason: real variation depresses the
// absolute score while leaving separation intact, so separation is what must
// gate acceptance.
//
// The value is set from both distributions, which is what the first version
// of this constant got wrong: 0.15 was chosen from the true gaps alone, and a
// floor justified only by what it accepts has never been tested from the side
// that matters. Re-measured against deliberate near-misses — rankBadgeRegion
// shifted off the sticky header across the four testdata fixtures, 16 vertical
// offsets from -160px to +160px and 8 horizontal, plus a flat frame and a
// white-noise frame — the rejected population tops out at:
//
//	blank frame                          gap 0.000 (every template ties at 0)
//	white noise                          gap 0.021
//	most off-header offsets              gap 0.002 .. 0.109
//	dy=-60 / dy=-40, best R4 0.717       gap 0.162  <- the worst, and WRONG rank
//
// 0.162 is above the old 0.15, so that floor demonstrably accepted a measured
// wrong-rank read. The smallest true gap is R3's cross-capture 0.250, so 0.20
// is the midpoint of the two distributions: 0.038 above the worst measured
// near-miss and 0.050 below the smallest true read. That is less headroom on
// the true side than 0.15 had (0.100), and it is bought deliberately — an
// overtight floor sends a real frame to review, which is recoverable, while a
// loose one attributes a whole frame's rows to the wrong rank group, which is
// not.
//
// Limitation 1, and it is not fixable by moving this number: the same sweep
// found offsets that score a **perfect 1.000 at the wrong rank** — dy=+60 gives
// R3 1.000 (gap 0.320) and dy=+120 gives R2 1.000 (gap 0.308), both above every
// true cross-capture gap measured. Those are not near-misses in any noise
// sense. The fixtures derive from evidence frame 06, which shows all four rank
// groups collapsed and stacked, so a one-header shift lands the region on the
// *adjacent group's real badge* and matches it correctly. A gap check answers
// "did anything match", never "did the right thing match": nothing in this
// file can distinguish a correct read of the intended badge from a correct
// read of a badge 60px away. What would is a second, independent signal that
// the region is on the header it thinks it is — vision.Match already returns
// Center and Box and both are currently discarded — or the header-band bounds
// (hy0/hy1 in roster.go) constraining the search rather than a fixed rect.
// Neither is done here; the exposure is bounded in practice by roster_capture
// working one expanded group at a time (a collapsed stack of four adjacent
// badges is the normalization screen, not a frame that gets ingested), and it
// is recorded so the next person does not mistake this constant for a defence
// it cannot provide.
//
// Limitation 2, carried from the first version: only R2, R3 and R4 have a
// cross-capture probe (capture 1's real frames, docs/superpowers/specs/
// evidence/m4-ocr-2026-08-14, which never advanced past R2 in its 62 frames —
// R1 does not appear in any real capture pulled so far). R1's separation is
// inferred from evidence frame 06's own within-frame cross-rank matrix (worst
// off-diagonal 0.463, an R2-vs-R3 pair, not R1's own), not measured against a
// second capture. A capture that reaches R1 would settle it; until then, R1
// rides on the same threshold the other three earned by measurement.
//
// TestRankBadgeMinGapSeparatesBothDistributions pins this value against both
// bounds, because the previous one was silently weakenable: with the gap check
// intact the whole internal/ingest suite still passed at 0.001.
const rankBadgeMinGap = 0.20

// rankTemplate is one rank badge's digit-only crop and the rank string it
// stands for, in the "R\d+" shape groupKey has always taken (see
// groupHeaderRe's old doc comment) — NCC now supplies this value instead of
// OCR, but nothing downstream (GroupTally's map key, the review queue, the
// per-group reconciliation loop) needed to change shape to receive it.
//
// The committed crops are 16x22, not the 18x22 Finding 4a measured. The
// deviation is recorded because it is not free: Finding 4a's own cross-capture
// R3 gap was 0.308 and these crops earn 0.250 on the same probe, so 2px
// narrower separates measurably worse — 0.058 of the 0.088 window between the
// near-miss and true distributions that rankBadgeMinGap has to fit inside. It
// is not re-cut here because the current value clears the floor with headroom
// on both sides and a recrop would invalidate every number in that constant's
// comment for a gain nothing currently needs. A capture that lands near the
// floor is the reason to revisit it.
type rankTemplate struct {
	Rank     string
	Template image.Image
}

var (
	rankTemplatesOnce sync.Once
	rankTemplates     []rankTemplate
	rankTemplatesErr  error
)

// rankBadgeOrder is fixed (not derived from a directory listing) so that
// loadRankTemplates and any test asserting on rankTemplates' order do not
// depend on filesystem enumeration order, which embed.FS does not guarantee
// across platforms.
var rankBadgeOrder = []string{"R1", "R2", "R3", "R4"}

// loadRankTemplates decodes the embedded digit-only badge crops once. An
// error here means the binary itself is broken (a template failed to embed
// or decode), not that one frame's rank is unreadable — every call after the
// first replays the same result via sync.Once, so a corrupt embed fails
// every rank match identically rather than intermittently.
func loadRankTemplates() ([]rankTemplate, error) {
	rankTemplatesOnce.Do(func() {
		for _, rank := range rankBadgeOrder {
			name := "rankbadges/" + rankFileStem(rank) + ".png"
			raw, err := rankBadgeFS.ReadFile(name)
			if err != nil {
				rankTemplatesErr = fmt.Errorf("ingest: reading embedded rank template %s: %w", name, err)
				return
			}
			img, err := png.Decode(bytes.NewReader(raw))
			if err != nil {
				rankTemplatesErr = fmt.Errorf("ingest: decoding embedded rank template %s: %w", name, err)
				return
			}
			rankTemplates = append(rankTemplates, rankTemplate{Rank: rank, Template: img})
		}
	})
	return rankTemplates, rankTemplatesErr
}

// rankFileStem lowercases "R3" to "r3" — the embedded filenames are
// lowercase because every other template file in this repo (templates/
// under the runtime's own manifest) is, and there is no reason for this
// package's private set to differ.
func rankFileStem(rank string) string {
	out := make([]byte, len(rank))
	for i := 0; i < len(rank); i++ {
		c := rank[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// rankMatch is one frame's rank-badge read: the winning template, its score,
// and its gap over the runner-up — the quantity rankBadgeMinGap actually
// gates (see that constant's doc comment for why the score alone never
// gates acceptance).
type rankMatch struct {
	Rank  string
	Score float64
	Gap   float64
}

// matchRankBadge is rank-badge NCC matching behind a package-level variable,
// purely so a test can script a rank without drawing a real badge sprite
// onto a synthetic frame — the same seam visionPreprocess uses for OCR
// (readField's own doc comment). Production always resolves to
// matchRankBadgeReal; only tests reassign it, and restore it after.
var matchRankBadge = matchRankBadgeReal

// scoredRank is one rank template's raw NCC score against a frame's badge
// region, with no acceptance rule applied yet.
type scoredRank struct {
	rank  string
	score float64
}

// bestTwoRankScores scores every rank template against img's badge region
// and returns the top two, best first. It applies no acceptance rule at all
// — rankBadgeMinGap is matchRankBadgeReal's concern, not this function's —
// so a test can call it directly to check what the gap check in
// matchRankBadgeReal is actually gating, as opposed to some other failure
// (a decode error, a region too small to admit a placement) that would
// refuse for an unrelated reason. See TestMatchRankBadgeRefusesAnUnconvincingMatch's
// own doc comment for why that distinction is the whole point of that test.
func bestTwoRankScores(img image.Image) (best, runnerUp scoredRank, err error) {
	best, runnerUp, _, err = bestTwoRankScoresIn(img, rankBadgeRegion)
	return best, runnerUp, err
}

// bestTwoRankScoresIn is bestTwoRankScores with the search region supplied,
// and it also returns where the winner was found.
//
// Both additions exist for the same caller. findHeaderCards has to look for
// rank badges in the MEMBER LIST, not only in the sticky band, because a
// frame can carry a second group's header card partway down it -- and once
// the region is a parameter, the winner's position is the only thing that says
// WHICH rows that card owns. rankBadgeMinGap's own doc comment names the
// discarded Center/Box as the missing second signal that a match is on the
// header it thinks it is; this is the caller that needs it.
func bestTwoRankScoresIn(img image.Image, region transport.Rect) (best, runnerUp scoredRank, bestBox transport.Rect, err error) {
	templates, err := loadRankTemplates()
	if err != nil {
		return scoredRank{}, scoredRank{}, transport.Rect{}, err
	}

	var results []scoredRank
	boxes := map[string]transport.Rect{}
	for _, rt := range templates {
		res, err := vision.Match(img, rt.Template, region, rankBadgeRefHeight)
		if err != nil {
			return scoredRank{}, scoredRank{}, transport.Rect{}, fmt.Errorf("ingest: matching rank badge template %s: %w", rt.Rank, err)
		}
		results = append(results, scoredRank{rank: rt.Rank, score: res.Score})
		boxes[rt.Rank] = res.Box
	}
	if len(results) < 2 {
		// Structurally unreachable with the four embedded templates above,
		// but a gap is meaningless with fewer than two candidates to compare
		// — refuse loudly rather than let a one-template "gap" of 0
		// silently pass or fail on arithmetic it was never designed for.
		return scoredRank{}, scoredRank{}, transport.Rect{}, fmt.Errorf("ingest: only %d rank templates loaded, need at least 2 to compute a gap: %w", len(results), ErrNoConfidentRank)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	return results[0], results[1], boxes[results[0].rank], nil
}

// matchRankBadgeReal scores every rank template against img's badge region
// and returns the argmax, but only if it leads the runner-up by at least
// rankBadgeMinGap. Rank is deliberately not supplied by OCR at all (see
// this file's own doc comments and CLAUDE.md's note on outlined game
// glyphs): the badge is drawn the same way the member-count glyphs are, and
// Finding 4 established that tesseract returns nothing usable from either
// shape under any PSM or charset tried.
func matchRankBadgeReal(img image.Image) (rankMatch, error) {
	best, runnerUp, err := bestTwoRankScores(img)
	if err != nil {
		return rankMatch{}, err
	}
	gap := best.score - runnerUp.score
	if gap < rankBadgeMinGap {
		return rankMatch{}, fmt.Errorf(
			"ingest: rank badge best match %s scored %.3f, runner-up %s scored %.3f (gap %.3f, below the required %.2f): %w",
			best.rank, best.score, runnerUp.rank, runnerUp.score, gap, rankBadgeMinGap, ErrNoConfidentRank)
	}
	return rankMatch{Rank: best.rank, Score: best.score, Gap: gap}, nil
}
