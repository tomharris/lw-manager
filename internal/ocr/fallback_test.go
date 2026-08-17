package ocr

import (
	"context"
	"image"
	"image/png"
	"os"
	"strconv"
	"strings"
	"testing"
)

// testdata/psm7_layout_blind.png is a real name crop from capture 6 (the M4
// gate's capture), frame 4, the row reading "Bujangann". It is committed
// because it is the smallest complete statement of a defect that cost this
// project 12 members on its gate: the crop is bold black text on a light grey
// field, correctly framed, with generous margins, and tesseract's PSM 7
// returns the empty string for it.
//
// The cause is layout analysis, not recognition. PSM 3/4/6/7/11/12 all return
// nothing; PSM 8 ("single word") and PSM 13 ("raw line, bypassing hacks that
// are Tesseract-specific") both read it. 13 is the one worth having, because 8
// would collapse the letter-spaced names ("M I C H E L L") that
// roster.Normalize exists to handle.
func loadLayoutBlindCrop(t *testing.T) image.Image {
	t.Helper()
	fh, err := os.Open("testdata/psm7_layout_blind.png")
	if err != nil {
		t.Fatalf("opening the layout-blind fixture: %v", err)
	}
	defer fh.Close()
	img, err := png.Decode(fh)
	if err != nil {
		t.Fatalf("decoding the layout-blind fixture: %v", err)
	}
	return img
}

// TestRawLinePSMRecoversALayoutBlindCrop asserts both halves of the defect at
// once, because either alone is misleading. The first half is the canary: if
// it ever fails, tesseract's layout analysis has learned to read this crop and
// the retry that ingest performs can be reconsidered rather than carried
// forever.
func TestRawLinePSMRecoversALayoutBlindCrop(t *testing.T) {
	eng := NewTesseractEngine()
	if !eng.Available() {
		t.Skip("tesseract not installed; skipping real-engine test (see CLAUDE.md)")
	}
	img := loadLayoutBlindCrop(t)
	ctx := context.Background()

	plain, err := eng.Read(ctx, img, Spec{})
	if err != nil {
		t.Fatalf("Read without fallback: %v", err)
	}
	if got := strings.TrimSpace(plain.Text); got != "" {
		t.Errorf("PSM %d read %q from the layout-blind fixture, want the empty string.\n"+
			"This is good news, not a regression: tesseract now reads a crop it used to reject, "+
			"so the ingest-level retry may no longer be needed. Re-measure with `make probe-m4` before removing it.",
			eng.PSM, got)
	}

	rawLine, err := eng.Read(ctx, img, Spec{PSM: PSMRawLine})
	if err != nil {
		t.Fatalf("Read at raw-line PSM: %v", err)
	}
	if !strings.Contains(strings.ToLower(rawLine.Text), "bujangann") {
		t.Errorf("raw-line read %q, want it to contain %q", rawLine.Text, "Bujangann")
	}
}

// Spec.PSM must override the engine's mode, and its zero value must leave the
// engine's own default alone. Both halves matter: the override is what makes a
// retry read possible at all, and the zero value is what keeps every field that
// does not ask for one reading exactly as it did before.
func TestSpecPSMOverridesTheEngineDefault(t *testing.T) {
	base := tesseractArgs("/tmp/x.png", PSMSingleLine, Spec{})
	if got := psmOf(t, base); got != PSMSingleLine {
		t.Errorf("zero Spec.PSM produced --psm %d, want the engine's %d", got, PSMSingleLine)
	}

	eng := NewTesseractEngine()
	if !eng.Available() {
		t.Skip("tesseract not installed; skipping real-engine test (see CLAUDE.md)")
	}
	img := loadLayoutBlindCrop(t)

	// The same engine, the same pixels, two Specs: only the PSM differs, and
	// only the one that asks for raw line reads anything.
	blind, err := eng.Read(context.Background(), img, Spec{})
	if err != nil {
		t.Fatalf("Read with the engine default: %v", err)
	}
	seeing, err := eng.Read(context.Background(), img, Spec{PSM: PSMRawLine})
	if err != nil {
		t.Fatalf("Read with Spec.PSM: %v", err)
	}
	if strings.TrimSpace(blind.Text) == strings.TrimSpace(seeing.Text) {
		t.Errorf("Spec.PSM had no effect: both reads returned %q", blind.Text)
	}
}

// psmOf extracts the --psm value from an argument vector.
func psmOf(t *testing.T, args []string) int {
	t.Helper()
	for i, a := range args {
		if a == "--psm" && i+1 < len(args) {
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				t.Fatalf("--psm value %q is not a number", args[i+1])
			}
			return n
		}
	}
	t.Fatalf("no --psm in %v", args)
	return 0
}
