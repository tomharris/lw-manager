package roster

import "testing"

// Every pair below is a real (stored name, OCR read) pair measured against
// capture 6, with the score the matcher gave before confusable-aware scoring
// existed. They are named rather than reduced to a table of characters because
// the point is not that "8 looks like S" in the abstract — it is that this
// alliance contains a member called ALBAN80 whose row OCR reads as ALBANSO,
// every week, and the pipeline must attribute it without a human.
func TestConfusableReadsAutoAccept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored string
		read   string
		was    int // the score before confusable-aware scoring
	}{
		{"digit-zero and letter-O, digit-eight and S", "ALBAN80", "ALBANSO", 71},
		{"digit-nine and S, digit-one and I", "AA91AA", "AASIAA", 66},
		{"digit-eight and S across a space", "Mar 89", "Mars9", 80},
		{"digit-six and G, digit-eight and S", "19GENERALS68", "19GENERALSGS", 83},
		{"roman numerals against lowercase L", "AnthraxVIII", "AnthraxVIIl", 90},
		{"a short name where one edit is fatal", "JPA9", "JPAS", 75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := TokenSetRatio(tc.read, tc.stored)
			if got < AutoAccept {
				t.Errorf("TokenSetRatio(%q, %q) = %d, want >= %d (was %d before confusable scoring)",
					tc.read, tc.stored, got, AutoAccept, tc.was)
			}
		})
	}
}

// The guard that matters more than any of the above. Confusable scoring raises
// the score of reads that are genuinely the same name; it must not raise the
// score of two names that are genuinely different. A row attributed to the
// wrong member writes one member's score onto another's row, which — unlike a
// row parked in the review queue — cannot be undone by a human noticing later.
func TestConfusableScoringStillSeparatesDifferentNames(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"kalor13", "kain445"},
		{"Zebra", "Zero Orca"},
		{"Beangraff", "Bujangann"},   // both real members of this alliance
		{"Recon13Bravo", "ZeroOrca"}, // ditto
		{"MICHELL", "Nichoj"},        // ditto: differ by more than a glyph
	} {
		if got := TokenSetRatio(tc.a, tc.b); got >= AutoAccept {
			t.Errorf("TokenSetRatio(%q, %q) = %d, want < %d — distinct members must never auto-accept",
				tc.a, tc.b, got, AutoAccept)
		}
	}
}

// A confusable substitution must still cost something. If it were free, a name
// made entirely of confusable characters would match any other name of the
// same shape at 100 — "1I1" and "lil" are different members if the roster says
// they are, and the matcher must not claim certainty it does not have.
func TestConfusableSubstitutionIsCheapNotFree(t *testing.T) {
	if got := TokenSetRatio("ALBAN80", "ALBANSO"); got == 100 {
		t.Errorf("got 100 — a confusable read must score below an exact one, or the "+
			"review queue can never distinguish a certain match from a plausible one (%d)", got)
	}
	if exact := TokenSetRatio("ALBAN80", "ALBAN80"); exact != 100 {
		t.Errorf("an exact match scored %d, want 100", exact)
	}
}

// Confusable folding must not leak into equality of the normalized forms:
// Normalize's job is to produce a stable key for storage and lookup, and two
// different members must keep two different keys. Only the *scoring* is
// confusable-aware.
func TestNormalizeDoesNotFoldConfusables(t *testing.T) {
	if Normalize("ALBAN80") == Normalize("ALBANSO") {
		t.Errorf("Normalize folded a confusable pair to one key (%q); "+
			"that would collapse two members in the database, not merely score them alike",
			Normalize("ALBAN80"))
	}
}
