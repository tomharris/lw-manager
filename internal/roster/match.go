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

	stripped := ratio(na, nb)
	tokenSet := ratio(strings.Join(ta, " "), strings.Join(tb, " "))
	if tokenSet > stripped {
		return tokenSet
	}
	return stripped
}

// ratio scores two non-empty, already-normalized strings in 0..100 by
// Levenshtein distance relative to the longer string's length.
func ratio(a, b string) int {
	if a == b {
		return 100
	}
	d := levenshtein(a, b)
	longest := len(a)
	if len(b) > longest {
		longest = len(b)
	}
	score := 100 * (longest - d) / longest
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

// levenshtein is the standard two-row edit distance.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
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
