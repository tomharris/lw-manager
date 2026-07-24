package ocr

import (
	"context"
	"image"
	"testing"
)

func TestResultAcceptedGatesOnConfidence(t *testing.T) {
	spec := Spec{MinConf: 0.55}
	if !(Result{Text: "123", Confidence: 0.8}).Accepted(spec) {
		t.Error("0.8 ≥ 0.55 should be accepted")
	}
	if (Result{Text: "123", Confidence: 0.40}).Accepted(spec) {
		t.Error("0.40 < 0.55 should be rejected (routes to review queue)")
	}
}

func TestSpecCleanStripsChars(t *testing.T) {
	spec := Spec{Strip: ","}
	if got := spec.Clean("1,234,567"); got != "1234567" {
		t.Errorf("Clean: got %q, want 1234567", got)
	}
}

func TestFakeEngineReturnsScriptedResults(t *testing.T) {
	fake := &FakeEngine{Results: []Result{
		{Text: "first", Confidence: 0.9},
		{Text: "second", Confidence: 0.7},
	}}

	r1, _ := fake.Read(context.Background(), image.NewGray(image.Rect(0, 0, 1, 1)), Spec{})
	r2, _ := fake.Read(context.Background(), image.NewGray(image.Rect(0, 0, 1, 1)), Spec{})
	if r1.Text != "first" || r2.Text != "second" {
		t.Errorf("scripted order wrong: got %q then %q", r1.Text, r2.Text)
	}
}

func TestFakeEngineErrorsWhenExhausted(t *testing.T) {
	fake := &FakeEngine{}
	if _, err := fake.Read(context.Background(), image.NewGray(image.Rect(0, 0, 1, 1)), Spec{}); err == nil {
		t.Error("expected error when no scripted results remain")
	}
}
