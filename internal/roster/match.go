package roster

import (
	"sort"
	"strings"
)

// Thresholds from design doc §5. A confirmation in the review queue writes an
// alias, so tomorrow's identical misread matches directly and accuracy
// compounds instead of being re-tuned.
const (
	// AutoAccept is the score at or above which a match is taken without a
	// human.
	AutoAccept = 92
	// ReviewFloor is the score at or above which a match is offered for human
	// confirmation. Below it the row is rejected outright.
	ReviewFloor = 75
)

// Member is the matcher's view of a known member: an ID, a display name, and
// any aliases a human has previously confirmed.
type Member struct {
	ID      int64
	Name    string
	Aliases []string
}

// Candidate is one scored match.
type Candidate struct {
	MemberID int64
	Name     string
	Score    int
}

// TokenSetRatio scores two names in 0..100, ignoring token order. Input is
// normalized internally, so callers may pass raw text.
//
// The score is the max of two comparisons: the fully whitespace-stripped
// forms (Normalize's output, joined with no separator at all), and the
// tokens sorted and rejoined with a single space between them. The first
// comparison is what makes a letter-spaced OCR read like "M I C H E L L"
// match "MICHELL" -- collapsing the spacing is the whole point of this
// package's headline case. The second is what makes token order not matter,
// e.g. "beast the mq" matching "mq the beast".
//
// Stripping whitespace entirely has a cost, accepted deliberately rather
// than missed: two names differing only in where a space falls collide, so
// "ab cd" and "abcd" both score 100. That trade-off cannot be avoided by a
// smarter rule -- ignoring spacing noise inside one name and respecting
// spacing between two different names are the same operation looked at from
// opposite sides, so there is no heuristic that gets both. Two members whose
// names differ only in space placement is pathological; a letter-spaced name
// is observed reality on this roster. Do not add a heuristic here.
//
// A name that normalizes to empty (symbols or emoji only, all stripped) is
// never a match, including against another empty normalization: an empty
// string is not evidence of anything, so both empty scores 0, not 100.
func TokenSetRatio(a, b string) int {
	na, nb := Normalize(a), Normalize(b)
	if na == "" || nb == "" {
		return 0
	}

	ta, tb := strings.Fields(NormalizeTokens(a)), strings.Fields(NormalizeTokens(b))
	sort.Strings(ta)
	sort.Strings(tb)

	best := ratio(na, nb)
	if tokenSet := ratio(strings.Join(ta, " "), strings.Join(tb, " ")); tokenSet > best {
		best = tokenSet
	}

	// A third comparison, with leading and trailing non-ASCII runs removed
	// from both sides (see stripDecoration). It is taken as a MAXIMUM
	// alongside the other two rather than replacing either, and that is a
	// safety property, not a stylistic choice: a maximum can only raise a
	// score, so no pair that matched before can stop matching because of it.
	// The risk it does carry runs the other way -- raising the score of a
	// pair that should NOT match -- and that is what ClosestPairScore
	// measures. On capture 6 the closest pair is unmoved at 60.
	//
	// Guarded on the strings actually differing so an all-ASCII roster, which
	// is most of them, pays one comparison rather than three.
	da, db := stripDecoration(na), stripDecoration(nb)
	if da != na || db != nb {
		if da == "" || db == "" {
			return best
		}
		if deco := ratio(da, db); deco > best {
			best = deco
		}
	}
	return best
}

// ratio scores two non-empty, already-normalized strings in 0..100 by
// Levenshtein distance relative to the longer string's length.
//
// Distances are in tenths of an edit (see confusable.go), so the denominator
// is scaled to match. A substitution between two characters OCR interchanges
// costs a fraction of an edit rather than a whole one, which is what lets a
// member called ALBAN80 match a row read as "ALBANSO" without lowering the
// threshold that protects every other row.
//
// Lengths are counted in RUNES, not bytes. That is a correction, not a
// refinement: the distance has always been computed over []rune while the
// denominator used len(), so any name with a multi-byte character was scored
// against a denominator up to three times too large — which INFLATES the
// score. Measured on capture 6, the Korean name "한씨아저씨" scored 66 against
// the unrelated read "AKAZA" for no reason but this. Byte lengths made false
// near-misses on exactly the non-Latin names hardest to read, and any cost
// constant fitted against those numbers would have been fitted to an artifact.
func ratio(a, b string) int {
	if a == b {
		return 100
	}
	longest := len([]rune(a))
	if n := len([]rune(b)); n > longest {
		longest = n
	}
	d := levenshtein(a, b)
	score := 100 * (longest*fullEditCost - d) / (longest * fullEditCost)
	if score < 0 {
		score = 0
	}
	return score
}

// Rank scores raw against every member, taking each member's best score across
// its display name and aliases, and returns them sorted best-first.
func Rank(raw string, members []Member) []Candidate {
	out := make([]Candidate, 0, len(members))
	for _, m := range members {
		best := TokenSetRatio(raw, m.Name)
		for _, a := range m.Aliases {
			if s := TokenSetRatio(raw, a); s > best {
				best = s
			}
		}
		out = append(out, Candidate{MemberID: m.ID, Name: m.Name, Score: best})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// levenshtein is the standard two-row edit distance, in tenths of an edit.
//
// Insertion and deletion cost a full edit; substitution costs whatever
// substitutionCost says, which is less than a full edit for a pair OCR
// interchanges. Only substitution is cheapened, and deliberately so: a
// confusable pair is evidence that the engine saw the right glyph and named it
// wrongly, whereas an inserted or dropped character is evidence that it saw
// something that was not there, or missed something that was. Those are not
// the same claim and must not carry the same discount.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j * fullEditCost
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i * fullEditCost
		for j := 1; j <= len(rb); j++ {
			cur[j] = min3(
				cur[j-1]+fullEditCost,
				prev[j]+fullEditCost,
				prev[j-1]+substitutionCost(ra[i-1], rb[j-1]),
			)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
