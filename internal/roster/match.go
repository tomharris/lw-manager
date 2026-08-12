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
func TokenSetRatio(a, b string) int {
	ta, tb := strings.Fields(NormalizeTokens(a)), strings.Fields(NormalizeTokens(b))
	sort.Strings(ta)
	sort.Strings(tb)
	sa, sb := strings.Join(ta, ""), strings.Join(tb, "")
	if sa == "" && sb == "" {
		return 100
	}
	if sa == "" || sb == "" {
		return 0
	}
	if sa == sb {
		return 100
	}
	d := levenshtein(sa, sb)
	longest := len(sa)
	if len(sb) > longest {
		longest = len(sb)
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
