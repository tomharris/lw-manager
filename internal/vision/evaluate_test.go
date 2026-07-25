package vision_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// buildRegistry writes a one-anchor manifest and loads it, so Evaluate is
// tested against a real Registry rather than a hand-built one.
func buildRegistry(t *testing.T, screen, anchorID string, region transport.Rect, threshold float64) *vision.Registry {
	t.Helper()
	manifest := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := vision.WriteAnchor(manifest, 40, vision.AnchorSpec{
		Screen: screen, ID: anchorID, Region: region,
		Threshold: threshold, IdentifiesScreen: true,
	}, tinyPNG(t)); err != nil {
		t.Fatalf("WriteAnchor: %v", err)
	}
	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	return reg
}

func TestEvaluateProducesOnePredictionPerFrame(t *testing.T) {
	reg := buildRegistry(t, "base", "base_button",
		transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 0.9)

	frames := []vision.Frame{
		{Hash: "a", Label: "base", Image: decodePNG(t, tinyFrame(t, 40, 40))},
		{Hash: "b", Label: vision.NoneLabel, Image: decodePNG(t, tinyFrame(t, 40, 40))},
	}

	preds, obs, err := vision.Evaluate(reg, frames)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(preds) != 2 {
		t.Fatalf("predictions = %d, want 2", len(preds))
	}
	if preds[0].Hash != "a" || preds[0].Label != "base" {
		t.Fatalf("prediction[0] = %+v, want hash a label base", preds[0])
	}
	// One anchor scored against both frames.
	if len(obs) != 2 {
		t.Fatalf("observations = %d, want 2", len(obs))
	}
	for _, o := range obs {
		if o.AnchorID != "base_button" || o.Screen != "base" {
			t.Fatalf("observation = %+v, want the base_button anchor", o)
		}
	}
}

func TestEvaluateReportsAnUnrecognizedFrameAsAnEmptyPrediction(t *testing.T) {
	// A threshold of 1.0 is unreachable, so nothing can be recognized.
	reg := buildRegistry(t, "base", "base_button",
		transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 1.0)

	preds, _, err := vision.Evaluate(reg, []vision.Frame{
		{Hash: "a", Label: "base", Image: decodePNG(t, tinyFrame(t, 40, 40))},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if preds[0].Predicted != "" {
		t.Fatalf("Predicted = %q, want empty for an unrecognized frame", preds[0].Predicted)
	}
}

// TestEvaluatePlumbsASuccessfulRecognitionThrough guards against an Evaluate
// that quietly drops the recognizer's result. The two tests above only check
// shape (right counts, right IDs) and the negative case (Predicted == ""); a
// gutted Evaluate that always returns Predicted: "" and fabricates plausible
// observations without ever calling Recognize or Match would pass both. This
// is the one that needs a frame which genuinely matches, so a stub that
// discards the real result actually gets caught.
func TestEvaluatePlumbsASuccessfulRecognitionThrough(t *testing.T) {
	const size = 60
	frameImg := decodePNG(t, tinyFrame(t, size, size))
	gray, ok := frameImg.(*image.Gray)
	if !ok {
		t.Fatalf("decoded frame is %T, want *image.Gray", frameImg)
	}

	// Cut the template directly out of the frame: it is then the same
	// pixels, not merely similar ones, so NCC at that placement is (up to
	// floating-point rounding) exactly 1.0 — a real match, not an
	// engineered coincidence.
	template := gray.SubImage(image.Rect(10, 10, 30, 30))
	var buf bytes.Buffer
	if err := png.Encode(&buf, template); err != nil {
		t.Fatalf("encoding cropped template: %v", err)
	}

	const threshold = 0.9 // comfortably below the ~1.0 an exact crop scores
	manifest := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := vision.WriteAnchor(manifest, size, vision.AnchorSpec{
		Screen: "base", ID: "base_button",
		Region:           transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1},
		Threshold:        threshold,
		IdentifiesScreen: true,
	}, buf.Bytes()); err != nil {
		t.Fatalf("WriteAnchor: %v", err)
	}
	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	preds, obs, err := vision.Evaluate(reg, []vision.Frame{
		{Hash: "a", Label: "base", Image: frameImg},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if preds[0].Predicted != "base" {
		t.Fatalf("Predicted = %q, want %q: Evaluate must plumb a genuine match through, not drop it",
			preds[0].Predicted, "base")
	}
	if len(obs) != 1 {
		t.Fatalf("observations = %d, want 1", len(obs))
	}
	if obs[0].Score <= threshold {
		t.Fatalf("observation score = %.4f, want > %.4f: a real match, not a zeroed-out stub",
			obs[0].Score, threshold)
	}
}

func TestRescaleChangesTheFrameHeight(t *testing.T) {
	src := decodePNG(t, tinyFrame(t, 40, 80))

	got := vision.Rescale(src, 0.5)

	if h := got.Bounds().Dy(); h != 40 {
		t.Fatalf("height = %d, want 40", h)
	}
	if w := got.Bounds().Dx(); w != 20 {
		t.Fatalf("width = %d, want 20", w)
	}
}

// tinyFrame builds a PNG with enough variation that NCC is well-defined.
func tinyFrame(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8((x*7 + y*13) % 256)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding frame: %v", err)
	}
	return buf.Bytes()
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding PNG: %v", err)
	}
	return img
}
