package roster

// Confusable-aware scoring: OCR's misreads, as distinct from unicode's
// lookalikes.
//
// normalize.go's foldHomoglyph handles the case where a *player* typed a
// character that is drawn like a Latin one — a Cyrillic "о", a Greek "Ο". That
// fold happens before any comparison, because the stored and read forms would
// otherwise share no characters at all and no threshold could help.
//
// This file handles the opposite direction: the player typed exactly what the
// roster records, and the *engine* returned a different character because the
// two are drawn alike at the size and weight this game renders names. Measured
// against capture 6, that is a member called ALBAN80 read as "ALBANSO", AA91AA
// read as "AASIAA", 19GENERALS68 read as "19GENERALSGS".
//
// The difference matters for where the fix goes. A homoglyph is folded away,
// so both forms collapse to one key and the database stores one member. A
// confusable is NOT folded: ALBAN80 and ALBANSO must remain distinct keys, or
// two members could collapse into one row. Only the *score* treats them as
// near-equal, and only when comparing a read against a candidate.
//
// The alternative — lowering AutoAccept — was considered and rejected. The
// threshold is what stops a misread row being attributed to the wrong member,
// and a wrong attribution writes one member's score onto another's row: unlike
// a queued row, that is not recoverable by a human noticing later. A targeted
// distance raises the score of reads that are genuinely the same name; a lower
// threshold raises the score of everything.

// Edit costs, in tenths of a full edit. Integer tenths rather than floats so
// the distance stays exact and ratio's percentage arithmetic keeps the shape
// it always had.
//
// confusableCost is fitted, not chosen. The binding cases are the short names,
// where a single edit is proportionally huge: AA91AA is six characters with
// two confusable substitutions, and needs 100*(60-2c)/60 >= 92, i.e. c <= 2.4.
// ALBAN80/ALBANSO gives c <= 2.8 by the same arithmetic. Anything above 2
// leaves those two members unmatched, so 2 is the largest value that does the
// job — deliberately the *least* aggressive setting that clears the cases,
// rather than the smallest number that clears them by the widest margin.
//
// Re-fit with `make probe-m4` if this ever moves. Do not lower it to chase a
// stubborn member without checking closestPairScore below.
const (
	fullEditCost   = 10
	confusableCost = 2
)

// confusablePairs are unordered character pairs OCR interchanges at this
// game's name rendering. Every entry is either a textbook OCR confusion or one
// measured directly against capture 6; the measured ones are marked.
//
// Kept deliberately short. Each pair is a small licence for two different
// members to score alike, so the set is a liability that has to be paid for by
// evidence, not a dictionary to be completed. Characters here are post-
// Normalize, which is why they are all lowercase or digits.
var confusablePairs = [][2]rune{
	// The classic identity-level confusions: at UI sizes these are the same
	// glyph, and no amount of preprocessing separates them.
	{'0', 'o'},
	{'1', 'i'},
	{'1', 'l'},
	{'i', 'l'}, // measured: AnthraxVIII -> "AnthraxVIIl"

	// Digit/letter pairs this font draws with the same skeleton.
	{'5', 's'},
	{'8', 's'}, // measured: ALBAN80 -> "ALBANSO", Mar 89 -> "Mars9"
	{'9', 's'}, // measured: AA91AA -> "AASIAA", JPA9 -> "JPAS"
	{'6', 'g'}, // measured: 19GENERALS68 -> "19GENERALSGS"
	{'9', 'g'},
	{'8', 'b'},
	{'6', 'b'},
	{'2', 'z'},

	// Letter pairs distinguished only by a stroke that thins out at this
	// weight.
	{'t', 'f'},
	{'c', 'e'},
	{'u', 'v'},
}

// confusable reports whether a and b are an interchangeable pair.
var confusable = func() map[[2]rune]bool {
	m := make(map[[2]rune]bool, len(confusablePairs)*2)
	for _, p := range confusablePairs {
		m[[2]rune{p[0], p[1]}] = true
		m[[2]rune{p[1], p[0]}] = true
	}
	return m
}()

// substitutionCost is the edit cost of replacing a with b: nothing when they
// are equal, a fraction of an edit when OCR interchanges them, a full edit
// otherwise.
func substitutionCost(a, b rune) int {
	switch {
	case a == b:
		return 0
	case confusable[[2]rune{a, b}]:
		return confusableCost
	default:
		return fullEditCost
	}
}

// ClosestPairScore returns the highest TokenSetRatio between any two distinct
// names in the list, with the pair that produced it. It is the guard that
// makes confusable scoring safe to tune: the risk of cheapening substitutions
// is that two real members score alike, and this measures that directly on a
// real roster instead of arguing about it.
//
// A result at or above AutoAccept means the roster contains two members the
// matcher can no longer tell apart, and the cost constant is too low — or those
// two members are genuinely indistinguishable to OCR, which is worth knowing
// either way, because no threshold can fix it and only an alias can.
func ClosestPairScore(names []string) (int, string, string) {
	best, bestA, bestB := 0, "", ""
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if s := TokenSetRatio(names[i], names[j]); s > best {
				best, bestA, bestB = s, names[i], names[j]
			}
		}
	}
	return best, bestA, bestB
}
