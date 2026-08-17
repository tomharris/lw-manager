package ocr

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// The argument vector is tested directly rather than through Read, because
// Read's observable output is whatever tesseract makes of the pixels — a test
// that asserted on recognized text could not tell "the -l flag was omitted"
// from "the language pack read this glyph badly". The flag is the thing under
// test, so the flag is what is asserted.

func TestTesseractArgsOmitsLanguageWhenUnset(t *testing.T) {
	args := tesseractArgs("/tmp/x.png", 7, Spec{})
	if slices.Contains(args, "-l") {
		t.Errorf("args = %v, want no -l when Spec.Languages is empty", args)
	}
}

func TestTesseractArgsPassesLanguages(t *testing.T) {
	args := tesseractArgs("/tmp/x.png", 7, Spec{Languages: "eng+kor+ara"})
	i := slices.Index(args, "-l")
	if i < 0 {
		t.Fatalf("args = %v, want a -l flag", args)
	}
	if got := args[i+1]; got != "eng+kor+ara" {
		t.Errorf("-l value = %q, want %q", got, "eng+kor+ara")
	}
}

// The charset whitelist and the language list are independent: a digits field
// constrains characters and stays on English, a name field does the reverse.
// Nothing in the argument builder should couple them.
func TestTesseractArgsCarriesCharsetAndLanguagesIndependently(t *testing.T) {
	args := tesseractArgs("/tmp/x.png", 7, Spec{Charset: "0123456789", Languages: "eng"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "tessedit_char_whitelist=0123456789") {
		t.Errorf("args = %v, want the charset whitelist preserved", args)
	}
	if !strings.Contains(joined, "-l eng") {
		t.Errorf("args = %v, want the language list preserved", args)
	}
}

// "tsv" is a tesseract *config name*, not a flag value, and it must stay the
// final positional argument: anything appended after it is parsed as another
// config file name and tesseract exits with a usage error. This is the kind of
// ordering constraint that survives refactoring only if something asserts it.
func TestTesseractArgsKeepsTSVLast(t *testing.T) {
	for _, spec := range []Spec{
		{},
		{Charset: "0123456789"},
		{Languages: "eng+kor"},
		{Charset: "0123456789", Languages: "eng+kor"},
	} {
		args := tesseractArgs("/tmp/x.png", 7, spec)
		if got := args[len(args)-1]; got != "tsv" {
			t.Errorf("spec %+v: last arg = %q, want \"tsv\" (args %v)", spec, got, args)
		}
	}
}

// InstalledLanguages and MissingLanguages exist so a caller that requires a
// language pack can say which one is absent. That matters because a missing
// pack is not an error from tesseract's point of view: it falls back silently
// and returns a worse read, which is expensive to diagnose from output alone.
func TestInstalledLanguagesReportsWhatIsPresentAndAbsent(t *testing.T) {
	eng := NewTesseractEngine()
	if !eng.Available() {
		t.Skip("tesseract not installed; skipping real-engine test (see CLAUDE.md)")
	}
	ctx := context.Background()

	langs, err := eng.InstalledLanguages(ctx)
	if err != nil {
		t.Fatalf("InstalledLanguages: %v", err)
	}
	if !slices.Contains(langs, "eng") {
		t.Errorf("installed languages %v do not include eng; the project's own OCR "+
			"cannot work without it, so this is a broken install rather than a test failure", langs)
	}

	// A name no language pack will ever have, so the negative half is stable.
	missing, err := eng.MissingLanguages(ctx, "eng+definitely_not_a_language")
	if err != nil {
		t.Fatalf("MissingLanguages: %v", err)
	}
	if want := []string{"definitely_not_a_language"}; !slices.Equal(missing, want) {
		t.Errorf("MissingLanguages = %v, want %v", missing, want)
	}
}
