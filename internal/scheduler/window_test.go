package scheduler

import (
	"testing"
	"time"
)

func TestOfflineWindowDurationInRange(t *testing.T) {
	loc := time.UTC
	for id := int64(1); id <= 500; id++ {
		date := time.Date(2026, 7, 24, 12, 0, 0, 0, loc)
		start, end := OfflineWindow(id, date)
		dur := end.Sub(start)
		if dur < 5*time.Hour || dur > 7*time.Hour {
			t.Fatalf("account %d: duration %v outside [5h,7h]", id, dur)
		}
		if start.Before(time.Date(2026, 7, 24, 0, 0, 0, 0, loc)) ||
			!start.Before(time.Date(2026, 7, 25, 0, 0, 0, 0, loc)) {
			t.Fatalf("account %d: start %v not within its day", id, start)
		}
	}
}

func TestOfflineWindowDeterministic(t *testing.T) {
	date := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	s1, e1 := OfflineWindow(42, date)
	s2, e2 := OfflineWindow(42, date)
	if !s1.Equal(s2) || !e1.Equal(e2) {
		t.Fatal("same inputs produced different windows")
	}
}

func TestOfflineWindowDriftsDaily(t *testing.T) {
	d1 := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	s1, _ := OfflineWindow(42, d1)
	s2, _ := OfflineWindow(42, d2)
	// Compare start-of-day offsets, not absolute times.
	off1 := s1.Sub(time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC))
	off2 := s2.Sub(time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC))
	if off1 == off2 {
		t.Fatal("window start did not drift between consecutive days")
	}
}

func TestInOfflineWindowContainsAndExcludes(t *testing.T) {
	id := int64(7)
	date := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	start, end := OfflineWindow(id, date)

	mid := start.Add(end.Sub(start) / 2)
	if !inOfflineWindow(id, mid) {
		t.Fatalf("midpoint %v reported outside window [%v,%v]", mid, start, end)
	}
	if inOfflineWindow(id, start.Add(-time.Minute)) {
		t.Fatal("instant just before start reported inside")
	}
	if inOfflineWindow(id, end.Add(time.Minute)) {
		t.Fatal("instant just after end reported inside")
	}
}

func TestInOfflineWindowHandlesMidnightWrap(t *testing.T) {
	// Find an account whose window wraps past midnight, then assert a time
	// in the early-morning tail (owned by yesterday's window) is detected.
	loc := time.UTC
	day := time.Date(2026, 7, 24, 0, 0, 0, 0, loc)
	for id := int64(1); id <= 2000; id++ {
		start, end := OfflineWindow(id, day)
		if end.Day() == start.Day() {
			continue // does not wrap
		}
		tail := time.Date(2026, 7, 25, 0, 30, 0, 0, loc) // 00:30 next day
		if !tail.Before(end) {
			continue
		}
		if !inOfflineWindow(id, tail) {
			t.Fatalf("account %d: %v inside wrapped window [%v,%v] not detected", id, tail, start, end)
		}
		return
	}
	t.Skip("no wrapping window found in 2000 accounts (unlikely); widen search if this trips")
}
