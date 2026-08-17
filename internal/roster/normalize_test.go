package roster

import "testing"

func TestNormalizeCollapsesLetterSpacing(t *testing.T) {
	// Recon frame 03: this member's name renders letter-spaced on screen and
	// OCR reads the spaces. Collapsing them is the single highest-value
	// normalization step, because it turns an unmatchable string into an exact
	// match without any fuzzy scoring at all.
	if got, want := Normalize("M I C H E L L"), "michell"; got != want {
		t.Errorf("Normalize = %q, want %q", got, want)
	}
}

func TestNormalizeCasefolds(t *testing.T) {
	if Normalize("Kain445") != Normalize("KAIN445") {
		t.Error("normalization must be case-insensitive")
	}
}

func TestNormalizeStripsCombiningMarks(t *testing.T) {
	if got, want := Normalize("Zérö"), "zero"; got != want {
		t.Errorf("Normalize = %q, want %q", got, want)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	for _, s := range []string{"M I C H E L L", "Zérö", "  Kain445  ", "O Nyankopo N"} {
		once := Normalize(s)
		if twice := Normalize(once); once != twice {
			t.Errorf("Normalize(%q) not idempotent: %q then %q", s, once, twice)
		}
	}
}

// The package doc has always claimed normalization handles homoglyphs, and it
// did not: NFKD leaves a Greek or Cyrillic lookalike exactly where it found
// it, because those are distinct letters and no decomposition relates them to
// Latin. The first real M4 gate run is what surfaced it — the alliance's rank
// 1 renders as "ΔKΔŽΔ", OCR reads the perfectly reasonable "AKAZA", and the
// two could never match at any threshold, because "δkδzδ" and "akaza" share
// no characters at all.
func TestNormalizeFoldsHomoglyphsToLatin(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Capture 6, rank 1. Δ is used as a stylistic A throughout this
		// game's names, and is what OCR reads it as.
		{"ΔKΔŽΔ", "akaza"},
		// Capture 6, rank 66. NFKD does not decompose ł — it is a letter in
		// its own right, not l plus a combining mark.
		{"Syłar", "sylar"},
		// Cyrillic lookalikes, the classic confusable set. Every one of
		// these renders identically to its Latin counterpart.
		{"СВОРЕКМАХ", "cbopekmax"},
		// Greek lowercase lookalikes.
		{"ναρκο", "vapko"},
	} {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Folding is for characters that render identically to a Latin one. A script
// with its own distinct shapes is not a homoglyph and must survive, or two
// unrelated members collapse onto the same key — which would write one
// member's score onto another's row, the worst failure this pipeline has.
func TestNormalizeDoesNotFoldDistinctScripts(t *testing.T) {
	if Normalize("한씨아저씨") == "" {
		t.Error("Korean must not be folded away to nothing")
	}
	if Normalize("Danny 狂") == Normalize("Danny") {
		t.Error("a CJK decoration is not a homoglyph and must stay distinguishing")
	}
}

func TestNormalizeKeepsDistinctNamesDistinct(t *testing.T) {
	if Normalize("Kalor13") == Normalize("Kain445") {
		t.Error("normalization must not collapse genuinely different names")
	}
}
