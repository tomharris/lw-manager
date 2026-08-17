// Package ocr reads text from a preprocessed image region.
//
// It is split from package vision on purpose: everything in vision is pure Go
// with no external process, but the only CGO-free OCR path is the tesseract
// CLI as a subprocess (gosseract needs CGO; PaddleOCR needs onnxruntime, also
// CGO — see CLAUDE.md). Keeping the subprocess here means vision's fixture
// tests never depend on a binary, and this package's real-engine test simply
// skips when tesseract is absent.
//
// Per-field configuration matters more than a global one: a VS score is
// digits-and-commas, a member name is free text (design doc §3 FIELD_SPECS).
// Spec carries that per-field intent.
package ocr

import (
	"context"
	"fmt"
	"image"
	"strings"
)

// Result is one OCR read: the raw recognized text and the engine's confidence
// in [0,1]. Text is kept raw for provenance (invariant #4, append-only facts);
// stripping and casting happen downstream so the original read is never lost.
type Result struct {
	Text       string
	Confidence float64
}

// Accepted reports whether the read cleared the field's minimum confidence.
// A rejected read must go to the review queue, never straight to a leaderboard
// (invariant #5).
func (r Result) Accepted(spec Spec) bool {
	return r.Confidence >= spec.MinConf
}

// Spec is one field's OCR configuration.
type Spec struct {
	// Charset is the character whitelist handed to the engine to constrain
	// recognition (e.g. "0123456789," for a score). Empty means unconstrained.
	Charset string
	// Strip lists characters removed from the raw text before casting (e.g.
	// "," so "1,234" becomes "1234").
	Strip string
	// MinConf is the confidence a read must reach to be Accepted.
	MinConf float64
	// Languages is the tesseract language list for this field, in tesseract's
	// own "+"-joined form ("eng+kor+ara+chi_sim+jpn"). Empty means the
	// engine's default, which is English.
	//
	// It belongs on the field spec rather than on the engine because the two
	// fields this project reads want opposite things. A VS points field is
	// digits behind a character whitelist, and every extra language only adds
	// ways to misread a numeral. A member name is free text that this
	// alliance renders in Korean, Arabic, CJK and katakana, which an
	// English-only tesseract returns as the empty string at every
	// preprocessing setting — measured, 15 of 142 row bands.
	//
	// Note the interaction with Charset: a whitelist is expressed in
	// characters, so combining one with a non-Latin language is close to
	// meaningless. Nothing enforces that here, because the useful
	// combinations are per-field decisions and this type only carries them.
	Languages string

	// PSM overrides the engine's page-segmentation mode for this field. Zero
	// means the engine's own default (PSMSingleLine), which is right for
	// every field read directly.
	//
	// It exists for retry reads. Tesseract's layout analysis sometimes
	// rejects a crop outright, and every layout-analysing mode (3, 4, 6, 7,
	// 11, 12) then returns the empty string for text a human reads without
	// effort — see internal/ocr/testdata/psm7_layout_blind.png, and
	// PSMRawLine, which reads it. Whether to retry such a crop, and with what
	// preprocessing, is a decision only the caller can make; this field is
	// how the caller expresses the second attempt.
	PSM int
}

// Clean removes every character in spec.Strip from raw.
func (spec Spec) Clean(raw string) string {
	if spec.Strip == "" {
		return raw
	}
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(spec.Strip, r) {
			return -1
		}
		return r
	}, raw)
}

// OCREngine reads text from an image under a field spec. The image is expected
// to be already preprocessed (vision.Preprocess); the engine does recognition,
// not image cleanup.
type OCREngine interface {
	Read(ctx context.Context, img image.Image, spec Spec) (Result, error)
}

// FakeEngine returns scripted results in order, so ingest and task code can be
// tested with no tesseract binary and no device. This is the replay-before-real
// discipline the project follows everywhere.
type FakeEngine struct {
	Results []Result
	idx     int
}

// Read returns the next scripted result, or an error once they run out.
func (f *FakeEngine) Read(_ context.Context, _ image.Image, _ Spec) (Result, error) {
	if f.idx >= len(f.Results) {
		return Result{}, fmt.Errorf("ocr: fake engine exhausted after %d results", len(f.Results))
	}
	r := f.Results[f.idx]
	f.idx++
	return r, nil
}
