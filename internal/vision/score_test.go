package vision_test

import (
	"math"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/vision"
)

func TestScoreCountsAPositiveCorrectOnlyOnAnExactMatch(t *testing.T) {
	r := vision.Score([]vision.Prediction{
		{Hash: "a", Label: "alliance", Predicted: "alliance"},
		{Hash: "b", Label: "alliance", Predicted: "alliance_tech"},
		{Hash: "c", Label: "mail", Predicted: ""},
	})

	if r.Total != 3 {
		t.Fatalf("Total = %d, want 3", r.Total)
	}
	if r.Correct != 1 {
		t.Fatalf("Correct = %d, want 1", r.Correct)
	}
	if math.Abs(r.Accuracy()-1.0/3.0) > 1e-9 {
		t.Fatalf("Accuracy = %v, want 1/3", r.Accuracy())
	}
}

// A negative is correct exactly when recognition failed. This is the rule
// that stops a loose threshold from passing the gate.
func TestScoreCountsANegativeCorrectOnlyWhenNothingWasRecognized(t *testing.T) {
	r := vision.Score([]vision.Prediction{
		{Hash: "a", Label: vision.NoneLabel, Predicted: ""},
		{Hash: "b", Label: vision.NoneLabel, Predicted: "radar"},
	})

	if r.Correct != 1 {
		t.Fatalf("Correct = %d, want 1", r.Correct)
	}
	if got := r.Matrix[vision.NoneLabel]["radar"]; got != 1 {
		t.Fatalf("Matrix[_none][radar] = %d, want 1", got)
	}
	if got := r.Matrix[vision.NoneLabel][vision.NonePrediction]; got != 1 {
		t.Fatalf("Matrix[_none][<none>] = %d, want 1", got)
	}
}

func TestScoreOnAnEmptyCorpusIsZeroAccuracyNotNaN(t *testing.T) {
	r := vision.Score(nil)

	if r.Total != 0 {
		t.Fatalf("Total = %d, want 0", r.Total)
	}
	if r.Accuracy() != 0 {
		t.Fatalf("Accuracy = %v, want 0", r.Accuracy())
	}
}

func TestScoreLabelsAndColumnsAreSortedWithNoneLast(t *testing.T) {
	r := vision.Score([]vision.Prediction{
		{Label: "radar", Predicted: "radar"},
		{Label: "alliance", Predicted: ""},
		{Label: vision.NoneLabel, Predicted: ""},
		{Label: "mail", Predicted: "mail"},
	})

	wantLabels := []string{"alliance", "mail", "radar", vision.NoneLabel}
	if len(r.Labels) != len(wantLabels) {
		t.Fatalf("Labels = %v, want %v", r.Labels, wantLabels)
	}
	for i, l := range wantLabels {
		if r.Labels[i] != l {
			t.Fatalf("Labels = %v, want %v", r.Labels, wantLabels)
		}
	}
	if r.Columns[len(r.Columns)-1] != vision.NonePrediction {
		t.Fatalf("Columns = %v, want %s last", r.Columns, vision.NonePrediction)
	}
}

// "94% accurate" is not actionable. "Eleven alliance_tech frames were called
// alliance" is.
func TestFormatMatrixNamesRowsColumnsAndCounts(t *testing.T) {
	r := vision.Score([]vision.Prediction{
		{Label: "alliance_tech", Predicted: "alliance"},
		{Label: "alliance_tech", Predicted: "alliance"},
		{Label: "alliance_tech", Predicted: "alliance_tech"},
	})

	out := r.FormatMatrix()
	if !strings.Contains(out, "alliance_tech") || !strings.Contains(out, "alliance") {
		t.Fatalf("matrix does not name its rows and columns:\n%s", out)
	}
	if !strings.Contains(out, "2") {
		t.Fatalf("matrix does not show the confusion count:\n%s", out)
	}
}
