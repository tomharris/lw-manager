package scheduler

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndCaps(t *testing.T) {
	c := time.Hour
	cases := map[int]time.Duration{
		0: 0,
		1: c,      // total spacing 2c
		2: 3 * c,  // total spacing 4c
		3: 7 * c,  // total spacing 8c
		5: 31 * c, // total spacing 32c
		9: 31 * c, // capped at failures=5
	}
	for failures, want := range cases {
		if got := backoff(c, failures); got != want {
			t.Errorf("backoff(1h, %d): got %v, want %v", failures, got, want)
		}
	}
}

func TestCadenceOffsetWithinBounds(t *testing.T) {
	c := time.Hour
	limit := time.Duration(0.2 * float64(c))
	seen := map[time.Duration]bool{}
	for i := 0; i < 300; i++ {
		last := time.Unix(int64(i*97), 0)
		off := cadenceOffset(int64(i), "help_all", last, c)
		if off < -limit || off > limit {
			t.Fatalf("offset %v outside ±20%% of %v", off, c)
		}
		seen[off] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct offsets in 300 draws — not jittered", len(seen))
	}
}

func TestCadenceOffsetDeterministic(t *testing.T) {
	c := time.Hour
	last := time.Unix(1000, 0)
	if cadenceOffset(5, "radar_quick", last, c) != cadenceOffset(5, "radar_quick", last, c) {
		t.Fatal("same inputs produced different offsets")
	}
	// A different lastRun moves the offset (drift across runs).
	if cadenceOffset(5, "radar_quick", last, c) == cadenceOffset(5, "radar_quick", last.Add(time.Second), c) {
		t.Fatal("offset did not move when lastRun changed")
	}
}
