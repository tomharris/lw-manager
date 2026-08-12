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

func TestNormalizeKeepsDistinctNamesDistinct(t *testing.T) {
	if Normalize("Kalor13") == Normalize("Kain445") {
		t.Error("normalization must not collapse genuinely different names")
	}
}
