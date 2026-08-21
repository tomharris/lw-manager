package ingest

import (
	"errors"
	"image"
	"sort"

	"github.com/tomharris/lw-manager/internal/transport"
)

// Rank-group header CARDS, found inside the member list rather than in the
// sticky band above it.
//
// WHY THIS EXISTS. IngestRoster used to attribute every row band on a frame
// to the rank its STICKY header names, and that is wrong on any frame where
// the list scrolls across a group boundary -- which, on a capture that walks
// the roster one group at a time, is every boundary frame there is. Capture 1
// shows both shapes:
//
//	seq 22  sticky R3 "Footloose 10/64", three R3 rows, then R2's own header
//	        CARD "I'm Alright 1/11" partway down the list, then R2's rows.
//	        Five R2 members were created under R3 by this.
//	seq  1  sticky R4 "This Is It 2/9" COLLAPSED, R3's header card immediately
//	        beneath it, then R3's rows. Four R3 members were created under R4,
//	        and creation is first-writer-wins, so the later R3 frames that read
//	        them correctly could not repair it.
//
// Those nine are exactly the roster gate's `wrong_group` count. The fix is to
// find the cards and partition the frame's bands at them.
//
// WHY THE CARD IS NOT SIMPLY ONE OF THE BANDS. SegmentRows never emits it. A
// header card is not one row pitch tall, so collectBands treats it as an
// interposed element: it extracts the longest run of pitch-spaced boundaries,
// then re-locks phase on what is left either side of that run, and the card
// falls in the gap BETWEEN the two runs. That is the right thing for
// segmentation and it is exactly what hides the card from attribution -- the
// mechanism that copes with the layout is the one that conceals the bug.
//
// WHY THE BADGE AND NOT THE TEXT. Same reason groupKey has always come from
// matchRankBadge: the rank badge is an outlined game glyph that OCR cannot
// read under any PSM or charset tried (Finding 4), and NCC reads it at 61/61
// on this capture. Looking for the card by its badge also means the search is
// the same one the sticky header already uses, at the same x band, with the
// same acceptance rule -- no second notion of "is that a header" to keep in
// step with the first.

// headerCardSearchY2 is how far down the frame a header card may be found, as
// a fraction of frame height.
//
// It extends BELOW memberListRegion.Y2 (0.89) deliberately, and R1 is why: on
// capture 1's late frames "R1 Danger Zone 0/12" sits at the very bottom of the
// list with its badge at y=1423..1463 of a 1600px frame, straddling
// memberListRegion's own bottom edge at y=1424. A search bounded by the list
// region would find that card on no frame of this capture at all, R1 would
// have no tally, and the roster gate's condition 4 would go on reporting that
// reconciliation describes a four-group capture with three groups in it.
//
// 0.93 is y=1488, which is past the card and short of the sticky alliance
// footer roster_capture's own region comment excludes. What bounds the risk of
// looking here is not this number but the acceptance rule: a rank badge has to
// beat every other rank template by rankBadgeMinGap before anything is
// attributed to it.
const headerCardSearchY2 = 0.93

// headerCardMinWindowFrac is the shortest uncovered gap worth searching, as a
// fraction of frame height. The rank templates are 22px tall at the 1600px
// reference height and vision.Match needs a region at least that tall to admit
// any placement at all, so a shorter window can only ever produce a refusal --
// skipping it saves four NCC passes per gap and, more usefully, keeps the
// per-frame diagnostic free of windows that were never candidates.
const headerCardMinWindowFrac = 30.0 / 1600

// headerCard is one rank-group header on a frame: which rank it names, and the
// band it occupies, in the same shape groupHeaderRegion states for the sticky
// one so both can be read by the same code.
type headerCard struct {
	Rank string
	// Band is the header's own rect -- groupHeaderRegion's X bounds, and the Y
	// bounds of this card rather than the sticky band's. It is what the name
	// and count reads are taken through.
	Band transport.Rect
	// Y0 is the top of Band in frame pixels, which is what a row band's own Y0
	// is compared against to decide which header owns it.
	Y0 int
	// InList distinguishes a card found in the member list from the sticky
	// header. Only the diagnostic uses it; attribution treats them alike, and
	// deliberately so -- a sticky header is just the card whose rows start at
	// the top of the region.
	InList bool

	Score, Gap float64
}

// findHeaderCards returns the rank-group header cards visible INSIDE the
// member list, in top-to-bottom order. The sticky header is not among them;
// IngestRoster prepends it.
//
// It searches the y-intervals that SegmentRows left uncovered, which is where
// a card must be: a card is not one pitch tall, so no band can contain one
// (collectBands' doc comment says why). The candidate windows are therefore
// the space above the first band, each gap between consecutive bands, and the
// space below the last band down to headerCardSearchY2.
//
// A window with no card in it produces no card: every candidate goes through
// the same rankBadgeMinGap rule matchRankBadgeReal applies to the sticky band,
// so "nothing here matched convincingly" is the default outcome rather than a
// special case. `make probe-roster PROBE_ARGS=-roster.headercards` reports
// what it finds per frame, and its `-roster.cardsinband` mode runs the same
// search over the INSIDE of each row band -- where a card cannot be, and where
// a member's avatar sits in the same x strip as the badge -- so the false
// positive rate is measured rather than assumed.
func findHeaderCards(img image.Image, bands []RowBand) ([]headerCard, error) {
	b := img.Bounds()
	h := b.Dy()
	if h <= 0 {
		return nil, nil
	}
	top := b.Min.Y + int(memberListRegion.Y1*float64(h))
	bottom := b.Min.Y + int(headerCardSearchY2*float64(h))

	sorted := append([]RowBand(nil), bands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Y0 < sorted[j].Y0 })

	var windows [][2]int
	cursor := top
	for _, band := range sorted {
		if band.Y0 > cursor {
			windows = append(windows, [2]int{cursor, band.Y0})
		}
		if band.Y1 > cursor {
			cursor = band.Y1
		}
	}
	if bottom > cursor {
		windows = append(windows, [2]int{cursor, bottom})
	}

	minWindow := int(headerCardMinWindowFrac * float64(h))
	halfBand := (groupHeaderRegion.Y2 - groupHeaderRegion.Y1) / 2

	var cards []headerCard
	for _, w := range windows {
		if w[1]-w[0] < minWindow {
			continue
		}
		region := transport.Rect{
			X1: rankBadgeRegion.X1,
			Y1: float64(w[0]-b.Min.Y) / float64(h),
			X2: rankBadgeRegion.X2,
			Y2: float64(w[1]-b.Min.Y) / float64(h),
		}
		best, runnerUp, box, err := bestTwoRankScoresIn(img, region)
		if err != nil {
			// A template that failed to embed or decode is a broken binary,
			// not an unreadable frame -- the same distinction matchRankBadge
			// draws. Propagate it; do not turn it into "no cards on this
			// frame", which would silently restore the old attribution on
			// every frame of every capture.
			if errors.Is(err, ErrNoConfidentRank) {
				continue
			}
			return nil, err
		}
		gap := best.score - runnerUp.score
		if gap < rankBadgeMinGap {
			continue
		}
		// The card's band is centred on the badge rather than taken from the
		// window: a window can be much taller than a card (the space below the
		// last band is 60px+), and the count read needs the band the glyphs
		// are actually in. The height is groupHeaderRegion's own, because a
		// card and the sticky header are the same element drawn in two places.
		cy := (box.Y1 + box.Y2) / 2
		card := headerCard{
			Rank:   best.rank,
			Band:   transport.Rect{X1: groupHeaderRegion.X1, Y1: cy - halfBand, X2: groupHeaderRegion.X2, Y2: cy + halfBand},
			InList: true,
			Score:  best.score,
			Gap:    gap,
		}
		card.Y0 = b.Min.Y + int(card.Band.Y1*float64(h))
		if !card.Band.Valid() {
			// A badge matched so close to the top or bottom of the frame that
			// the band around it leaves the unit square. Nothing can be read
			// through it, and attributing rows to a header whose own text
			// cannot be read is the blind tap invariant #3 forbids.
			continue
		}
		cards = append(cards, card)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Y0 < cards[j].Y0 })
	return cards, nil
}

// ownerOf returns the rank of the last header at or above a row band's top --
// the group whose rows that band is one of.
//
// headers must be sorted by Y0 with the sticky header first. The sticky header
// sits above memberListRegion entirely, so every band has an owner and there
// is no "no header" case to handle: a frame with no readable sticky rank never
// reaches here (IngestRoster sends it to review whole).
//
// The comparison is against the band's TOP. A band whose top is below a card
// is one of that card's rows; a band that begins above the card cannot be,
// whatever its bottom does, because a row that the card overlapped would not
// have been emitted as a band at all.
func ownerOf(headers []headerCard, band RowBand) string {
	rank := headers[0].Rank
	for _, hc := range headers[1:] {
		if hc.Y0 <= band.Y0 {
			rank = hc.Rank
			continue
		}
		break
	}
	return rank
}
