package ocr

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Page-segmentation modes this package names. Tesseract has more; these are
// the two with a reason to be referred to by name rather than by number.
const (
	// PSMSingleLine is tesseract's "treat the image as a single text line".
	// It is the default for every field here: each crop is one line by
	// construction, cut from a row band.
	PSMSingleLine = 7
	// PSMRawLine is "raw line" — a single text line with tesseract's layout
	// analysis bypassed. It reads crops that PSMSingleLine returns nothing
	// for, and is markedly worse on crops PSMSingleLine handles, so it earns
	// its place only as Spec.FallbackPSM.
	PSMRawLine = 13
)

// TesseractEngine reads text by shelling out to the `tesseract` CLI. This is
// the only CGO-free OCR path (CLAUDE.md): gosseract and PaddleOCR both need
// CGO, which the build forbids. The cost is a subprocess per read and a PNG
// round-trip through a temp file, which is fine at capture cadence.
type TesseractEngine struct {
	// Binary is the tesseract executable; empty means "tesseract" on PATH.
	Binary string
	// PSM is the page-segmentation mode. 7 = single text line, the mode the
	// reference alliance-manager used per-row (M0 §Corrections, PSM_SINGLE_LINE).
	PSM int
}

// NewTesseractEngine returns an engine with the single-line default.
func NewTesseractEngine() *TesseractEngine {
	return &TesseractEngine{PSM: 7}
}

// Available reports whether the tesseract binary can be found. Callers and
// tests use it to skip cleanly rather than fail when OCR isn't installed.
func (e *TesseractEngine) Available() bool {
	_, err := exec.LookPath(e.binary())
	return err == nil
}

func (e *TesseractEngine) binary() string {
	if e.Binary != "" {
		return e.Binary
	}
	return "tesseract"
}

// Read runs tesseract over img and returns the recognized text with an
// averaged confidence. img is expected to be preprocessed already
// (vision.Preprocess) — this engine does recognition, not image cleanup.
func (e *TesseractEngine) Read(ctx context.Context, img image.Image, spec Spec) (Result, error) {
	tmp, err := os.CreateTemp("", "ocr-*.png")
	if err != nil {
		return Result{}, fmt.Errorf("ocr: creating temp image: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := png.Encode(tmp, img); err != nil {
		tmp.Close()
		return Result{}, fmt.Errorf("ocr: encoding temp image: %w", err)
	}
	tmp.Close()

	psm := spec.PSM
	if psm == 0 {
		psm = e.PSM
	}
	if psm == 0 {
		psm = PSMSingleLine
	}
	return e.runPSM(ctx, tmp.Name(), psm, spec)
}

// runPSM executes one tesseract invocation over an already-written image file.
// Split out of Read so the fallback retries recognition without re-encoding
// the PNG — the temp file is the expensive part and it does not change
// between attempts.
func (e *TesseractEngine) runPSM(ctx context.Context, path string, psm int, spec Spec) (Result, error) {
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.binary(), tesseractArgs(path, psm, spec)...)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("ocr: tesseract failed at psm %d (%v): %s", psm, err, strings.TrimSpace(stderr.String()))
	}
	text, conf := parseTSV(out.String())
	return Result{Text: text, Confidence: conf}, nil
}

// tesseractArgs builds the command line for one read. It is a free function
// rather than inline in Read so the flags can be asserted without running a
// subprocess — the alternative is a test that reads pixels back and cannot
// distinguish a missing flag from a bad recognition.
//
// Argument order is load-bearing at the end: "tsv" is a config *name*, not a
// flag value, and tesseract parses anything after it as a further config file.
// It stays last.
func tesseractArgs(imagePath string, psm int, spec Spec) []string {
	args := []string{imagePath, "stdout", "--psm", strconv.Itoa(psm)}
	if spec.Languages != "" {
		args = append(args, "-l", spec.Languages)
	}
	if spec.Charset != "" {
		args = append(args, "-c", "tessedit_char_whitelist="+spec.Charset)
	}
	return append(args, "tsv") // tsv config gives per-word confidence
}

// InstalledLanguages returns the language packs tesseract can see, as reported
// by --list-langs. It exists so a caller that requires a pack can say which
// one is missing: a missing pack is not an error from tesseract's point of
// view, it silently falls back and returns a worse read, which is exactly the
// kind of quiet degradation that is expensive to diagnose from output alone.
func (e *TesseractEngine) InstalledLanguages(ctx context.Context) ([]string, error) {
	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.binary(), "--list-langs")
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ocr: listing tesseract languages (%v): %s", err, strings.TrimSpace(stderr.String()))
	}
	// The first line is a header ("List of available languages..."); the rest
	// are one language code each. Tesseract writes the header to stdout, not
	// stderr, so it has to be skipped by shape rather than by stream.
	var langs []string
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, ":") {
			continue
		}
		langs = append(langs, line)
	}
	return langs, nil
}

// MissingLanguages returns which of want are not installed, preserving order.
// want is tesseract's "+"-joined form, so a Spec.Languages value can be passed
// straight through.
func (e *TesseractEngine) MissingLanguages(ctx context.Context, want string) ([]string, error) {
	have, err := e.InstalledLanguages(ctx)
	if err != nil {
		return nil, err
	}
	installed := make(map[string]bool, len(have))
	for _, l := range have {
		installed[l] = true
	}
	var missing []string
	for _, l := range strings.Split(want, "+") {
		if l != "" && !installed[l] {
			missing = append(missing, l)
		}
	}
	return missing, nil
}

// parseTSV extracts the words and averages the confidences from tesseract's
// TSV output. Columns are tab-separated; conf is column 11 (0-indexed 10) and
// text the last, with conf -1 on non-text rows.
func parseTSV(tsv string) (string, float64) {
	var words []string
	var confSum float64
	var n int
	for i, line := range strings.Split(tsv, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // header, or trailing blank
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 12 {
			continue
		}
		conf, err := strconv.ParseFloat(cols[10], 64)
		if err != nil || conf < 0 {
			continue
		}
		word := strings.TrimSpace(cols[11])
		if word == "" {
			continue
		}
		words = append(words, word)
		confSum += conf
		n++
	}
	if n == 0 {
		return "", 0
	}
	return strings.Join(words, " "), confSum / float64(n) / 100
}

// TesseractEngine satisfies OCREngine.
var _ OCREngine = (*TesseractEngine)(nil)
