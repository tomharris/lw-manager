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

// stripDecoration exists for one observed shape: a name whose Latin core is
// wrapped in characters from another script that OCR cannot read back. These
// cases are drawn from capture 6's roster, not invented.
func TestStripDecorationKeepsTheLatinCore(t *testing.T) {
	cases := []struct{ in, want string }{
		// U+03DF GREEK SMALL LETTER KOPPA. Unicode says Ll, a letter, which
		// is exactly why Normalize keeps it and why this function is needed.
		{"ϟϟleoϟϟ", "leo"},
		// Arabic-Indic digits, Unicode Nd. Same story from the digit side.
		{"٣١٢ali٣١٢", "ali"},
		{"danny狂", "danny"},
		{"zap" + "ꙅઉ", "zap"},
		// Interior decoration is NOT stripped: only leading and trailing runs
		// are. A character between two Latin runs is far more likely to be a
		// homoglyph the fold missed than an ornament, and dropping it would
		// silently join two halves of a name that were never adjacent.
		{"ab狂cd", "ab狂cd"},
		// Already clean: unchanged, and cheap.
		{"gersongamer", "gersongamer"},
	}
	for _, c := range cases {
		if got := stripDecoration(c.in); got != c.want {
			t.Errorf("stripDecoration(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The guard that makes the whole thing safe. A name with no Latin core at all
// is returned untouched, because there is nothing to strip it TO -- stripping
// would leave the empty string, and TokenSetRatio scores an empty
// normalization as 0 against everything, so this member would stop matching
// its own clean read.
func TestStripDecorationLeavesAScriptOnlyNameAlone(t *testing.T) {
	for _, in := range []string{"한씨아저씨", "狂", "٣١٢"} {
		if got := stripDecoration(in); got != in {
			t.Errorf("stripDecoration(%q) = %q, want it untouched", in, got)
		}
	}
}

// Ordering is the load-bearing detail. A homoglyph is not decoration: the
// player typed a character that RENDERS like a Latin one, and foldHomoglyph
// turns it into that Latin one. Because stripping runs on Normalize's output,
// ΔKΔŽΔ has already become "akaza" -- all ASCII -- and nothing is stripped.
// Strip before folding instead and this name reduces to "K", which is both a
// destroyed match and a one-character string that would score alarmingly
// against short names.
func TestDecorationStrippingRunsAfterHomoglyphFolding(t *testing.T) {
	if got := stripDecoration(Normalize("ΔKΔŽΔ")); got != "akaza" {
		t.Errorf("ΔKΔŽΔ normalized+stripped = %q, want %q", got, "akaza")
	}
}

// TestArchFoldsToN pins the stylised-substitution fold and, more importantly,
// pins BOTH codepoints. The roster fixture's note is explicit that it cannot
// settle which one the glyph is, so a fold covering only one of them would
// work or not work depending on a reading the transcriber flagged as
// uncertain.
func TestArchFoldsToN(t *testing.T) {
	for _, name := range []string{"TYRIO∩", "TYRIOՈ"} {
		if got := Normalize(name); got != "tyrion" {
			t.Errorf("Normalize(%q) = %q, want %q", name, got, "tyrion")
		}
		if got := TokenSetRatio("TYRION", name); got < AutoAccept {
			t.Errorf("TokenSetRatio(%q, %q) = %d, want >= AutoAccept %d", "TYRION", name, got, AutoAccept)
		}
	}
}

// TestArchFoldDoesNotCollapseUnrelatedNames is the other half: the fold is a
// licence for two members to score alike, so it has to be shown NOT to reach
// past the one glyph it is for.
func TestArchFoldDoesNotCollapseUnrelatedNames(t *testing.T) {
	if got := TokenSetRatio("TYRIO∩", "TYRIOU"); got >= AutoAccept {
		t.Errorf("TokenSetRatio(%q, %q) = %d, which is at or above AutoAccept %d: the arch fold reaches too far",
			"TYRIO∩", "TYRIOU", got, AutoAccept)
	}
}
