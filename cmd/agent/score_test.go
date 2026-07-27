package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// variedPNG returns a small non-uniform PNG: enough real structure to pass
// WriteAnchor's variance check and to be a meaningful (if not necessarily
// correct) NCC template.
func variedPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8((x*37 + y*53) % 256)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding PNG: %v", err)
	}
	return buf.Bytes()
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// whatever was written to it. runScore writes its report and warnings
// straight to os.Stderr/os.Stdout, so this is the only way to observe them
// from a test without changing that CLI-results-on-stdout convention
// (CLAUDE.md: "All output goes through log/slog to stderr. CLI results go to
// stdout so they stay pipeable" — the score report and this warning are
// exactly that stderr side).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading pipe: %v", err)
	}
	return string(out)
}

// An operator running --apply-thresholds sees the accuracy/matrix/separation
// figures printed below the apply step, but those are computed from
// vision.Evaluate before SetThresholds ever runs — they describe the
// manifest as it was, not as it now is. Without a warning, "accuracy 0.99
// gate 0.98, exit 0" reads as a measurement of the just-tuned manifest, when
// it has not actually been re-measured.
func TestRunScoreWarnsThatApplyThresholdsFiguresPredateTheUpdate(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	const refHeight = 100

	spec := vision.AnchorSpec{
		Screen:           "base",
		ID:               "anchor",
		Region:           transport.Rect{X1: 0.1, Y1: 0.1, X2: 0.4, Y2: 0.4},
		Threshold:        0.1,
		IdentifiesScreen: true,
	}
	if err := vision.WriteAnchor(manifest, refHeight, spec, variedPNG(t, 10, 10)); err != nil {
		t.Fatalf("WriteAnchor: %v", err)
	}

	root := filepath.Join(dir, "corpus")
	store := corpus.New(root)
	if _, _, err := store.Add("base", variedPNG(t, refHeight, refHeight)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, _, err := store.Add(corpus.None, variedPNG(t, refHeight, refHeight)); err != nil {
		t.Fatalf("Add negative: %v", err)
	}

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runScore(context.Background(), config.Config{}, []string{
			"--corpus", root, "--templates", manifest, "--gate", "0", "--apply-thresholds",
		})
	})
	if runErr != nil {
		t.Fatalf("runScore: %v\nstderr:\n%s", runErr, stderr)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "BEFORE") {
		t.Fatalf("stderr does not warn that the figures predate the threshold update:\n%s", stderr)
	}
}
