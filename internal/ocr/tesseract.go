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

	psm := e.PSM
	if psm == 0 {
		psm = 7
	}
	args := []string{tmp.Name(), "stdout", "--psm", strconv.Itoa(psm)}
	if spec.Charset != "" {
		args = append(args, "-c", "tessedit_char_whitelist="+spec.Charset)
	}
	args = append(args, "tsv") // tsv config gives per-word confidence

	var out, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.binary(), args...)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("ocr: tesseract failed (%v): %s", err, strings.TrimSpace(stderr.String()))
	}

	text, conf := parseTSV(out.String())
	return Result{Text: text, Confidence: conf}, nil
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
