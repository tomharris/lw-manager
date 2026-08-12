package ingest

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrUnparseable reports that a field's raw OCR text did not have the shape
// its parser expects. It is a sentinel so callers can route the row to review
// rather than guessing a value.
var ErrUnparseable = errors.New("ingest: field could not be parsed")

var (
	powerRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*([KMB])`)
	levelRe = regexp.MustCompile(`([0-9]+)`)
	agoRe   = regexp.MustCompile(`([0-9]+)\s*([hmd])`)
)

// ParsePower reads the abbreviated power the member list shows, e.g.
// "Power: 216.2M".
//
// The game never shows full precision here, so the result carries at most four
// significant figures: 216.2M is 216,200,000 give or take 50,000. That is a
// property of the screen, recorded rather than worked around, and it is below
// the weekly deltas any derived metric cares about.
func ParsePower(s string) (int64, error) {
	m := powerRe.FindStringSubmatch(strings.ToUpper(s))
	if m == nil {
		return 0, fmt.Errorf("ingest: power %q: %w", s, ErrUnparseable)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("ingest: power %q: %w", s, ErrUnparseable)
	}
	switch m[2] {
	case "K":
		v *= 1e3
	case "M":
		v *= 1e6
	case "B":
		v *= 1e9
	}
	return int64(v), nil
}

// ParseLevel reads "Lv.35".
func ParseLevel(s string) (int, error) {
	m := levelRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("ingest: level %q: %w", s, ErrUnparseable)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("ingest: level %q: %w", s, ErrUnparseable)
	}
	return n, nil
}

// ParseLastActiveHours reads the relative last-active label and returns hours
// ago. "Online" is zero.
//
// Hours-ago is stored rather than a derived timestamp so the fact stays equal
// to what the screenshot shows, which is what makes it checkable against that
// screenshot later. Resolution is about an hour.
func ParseLastActiveHours(s string) (float64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	if strings.HasPrefix(t, "online") {
		return 0, nil
	}
	m := agoRe.FindStringSubmatch(t)
	if m == nil {
		return 0, fmt.Errorf("ingest: last-active %q: %w", s, ErrUnparseable)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("ingest: last-active %q: %w", s, ErrUnparseable)
	}
	switch m[2] {
	case "m":
		return n / 60, nil
	case "h":
		return n, nil
	case "d":
		return n * 24, nil
	}
	return 0, fmt.Errorf("ingest: last-active %q: %w", s, ErrUnparseable)
}

// ParsePoints reads a full-precision VS score such as "45,048,150". Unlike
// power, the ranking shows every digit.
func ParsePoints(s string) (int64, error) {
	t := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	if t == "" {
		return 0, fmt.Errorf("ingest: points %q: %w", s, ErrUnparseable)
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ingest: points %q: %w", s, ErrUnparseable)
	}
	return n, nil
}
