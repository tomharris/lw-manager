package runtime

import (
	"math/rand"
	"testing"
	"time"
)

func TestJitterStaysInBounds(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	min, max := 100*time.Millisecond, 300*time.Millisecond
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := Jitter(r, min, max)
		if d < min || d > max {
			t.Fatalf("jitter %v outside [%v, %v]", d, min, max)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Fatalf("jitter produced only %d distinct values in 200 draws — not jittered", len(seen))
	}
}

func TestJitterDegenerateRange(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	if d := Jitter(r, time.Second, time.Second); d != time.Second {
		t.Fatalf("equal bounds: got %v", d)
	}
	// min > max is a programming error; jitter must not panic or go negative.
	if d := Jitter(r, 2*time.Second, time.Second); d != 2*time.Second {
		t.Fatalf("inverted bounds: got %v, want clamp to min", d)
	}
}
