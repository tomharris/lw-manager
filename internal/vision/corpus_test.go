//go:build corpus

// The M1 gate: the screen recognizer scores at least 98% on the real corpus,
// offline, with no device attached.
//
// Behind a build tag because multi-scale NCC over 200+ frames is slow and
// `make test` must stay fast. Run it with `make gate`.
package vision_test

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/vision"
)

// gateAccuracy is the M1 phase gate from the platform design doc.
const gateAccuracy = 0.98

func TestM1RecognizerGate(t *testing.T) {
	corpusRoot := envOr("LW_CORPUS_ROOT", filepath.Join("..", "..", "fixtures", "corpus"))
	manifestPath := envOr("LW_TEMPLATES", filepath.Join("..", "..", "templates", "manifest.yaml"))

	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		t.Skipf("no template manifest at %s; crop anchors in `agent studio` first", manifestPath)
	}
	store := corpus.New(corpusRoot)
	files, err := store.All()
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	if len(files) == 0 {
		t.Skipf("no corpus at %s; run `agent corpus pull` first", corpusRoot)
	}

	reg, err := vision.LoadRegistry(manifestPath)
	if err != nil {
		t.Fatalf("loading registry: %v", err)
	}

	var frames []vision.Frame
	for _, f := range files {
		if f.Label == corpus.Unsorted {
			continue // no ground truth, nothing to score against
		}
		data, err := store.Read(f.Hash)
		if err != nil {
			t.Fatalf("reading %s: %v", f.Hash, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decoding %s (label %q): %v", f.Hash, f.Label, err)
		}
		frames = append(frames, vision.Frame{Hash: f.Hash, Label: f.Label, Image: img})
	}
	if len(frames) == 0 {
		t.Skip("corpus has no labeled frames yet; label them in `agent studio`")
	}

	preds, obs, err := vision.Evaluate(reg, frames)
	if err != nil {
		t.Fatalf("evaluating: %v", err)
	}
	report := vision.Score(preds)

	// A failure prints the diagnostics, not just the number. "94%" is not
	// actionable; the matrix and the separation report say what to fix.
	if report.Accuracy() < gateAccuracy {
		t.Errorf("M1 gate: accuracy %.4f (%d/%d) is below %.2f\n\n%s\n%s",
			report.Accuracy(), report.Correct, report.Total, gateAccuracy,
			report.FormatMatrix(), vision.FormatSeparations(vision.Separations(obs)))
	}

	// A corpus too uniform to be meaningful would pass the accuracy check
	// while proving nothing, so assert the shape of the corpus too.
	if report.Total < 200 {
		t.Errorf("corpus has %d labeled frames, want at least 200 (design doc line 376)", report.Total)
	}
	if n := len(report.Matrix[vision.NoneLabel]); n == 0 {
		t.Error("corpus has no negatives; without them a loose threshold passes this gate")
	}
	for _, label := range []string{
		"base", "world_map", "alliance", "alliance_tech",
		"alliance_members", "vs_ranking", "mail", "radar",
	} {
		if total := rowTotal(report.Matrix[label]); total < 10 {
			t.Errorf("label %q has only %d frames; too few to say anything", label, total)
		}
	}
}

func rowTotal(row map[string]int) int {
	n := 0
	for _, v := range row {
		n += v
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
