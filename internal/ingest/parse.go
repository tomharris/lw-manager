package ingest

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ErrUnparseable reports that a field's raw OCR text did not have the shape
// its parser expects. It is a sentinel so callers can route the row to review
// rather than guessing a value.
var ErrUnparseable = errors.New("ingest: field could not be parsed")

var (
	powerRe  = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?[KMB]$`)
	levelRe  = regexp.MustCompile(`^(?:lv\.?|lv\s)?\s*([0-9]+)\s*$`)
	agoRe    = regexp.MustCompile(`^([0-9]+)\s*([hmd])\s*(ago)?$`)
	pointsRe = regexp.MustCompile(`^(?:[0-9]{1,3}(?:,[0-9]{3})*|[0-9]+)$`)
)

// ParsePower reads the abbreviated power the member list shows, e.g.
// "Power: 216.2M".
//
// The game never shows full precision here, so the result carries at most four
// significant figures: 216.2M is 216,200,000 give or take 50,000. That is a
// property of the screen, recorded rather than worked around, and it is below
// the weekly deltas any derived metric cares about.
//
// An unparseable field returns ErrUnparseable so the row routes to review
// instead of contributing a confident wrong number to a leaderboard.
func ParsePower(s string) (int64, error) {
	// Strip optional "Power:" label and surrounding whitespace
	t := strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(t), "power:") {
		t = strings.TrimSpace(t[6:])
	}

	// Validate the entire remainder matches the expected shape
	upper := strings.ToUpper(t)
	if !powerRe.MatchString(upper) {
		return 0, fmt.Errorf("ingest: power %q: %w", s, ErrUnparseable)
	}

	// Extract the numeric part and suffix
	lastChar := upper[len(upper)-1]
	numPart := strings.TrimSpace(upper[:len(upper)-1])

	v, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("ingest: power %q: %w", s, ErrUnparseable)
	}

	switch lastChar {
	case 'K':
		v *= 1e3
	case 'M':
		v *= 1e6
	case 'B':
		v *= 1e9
	}
	return int64(math.Round(v)), nil
}

// ParseLevel reads "Lv.35" and validates the shape. It requires an "Lv"
// prefix (case-insensitive) to reject bare numbers that might appear in
// other fields and fail an OCR crop silently.
func ParseLevel(s string) (int, error) {
	t := strings.TrimSpace(s)
	lower := strings.ToLower(t)

	// Require "Lv" or "lv" prefix
	if !strings.HasPrefix(lower, "lv") {
		return 0, fmt.Errorf("ingest: level %q: %w", s, ErrUnparseable)
	}

	// Strip the prefix and any following separator (. or space)
	rest := strings.TrimSpace(t[2:])
	if len(rest) > 0 && (rest[0] == '.' || rest[0] == ' ') {
		rest = strings.TrimSpace(rest[1:])
	}

	// Extract just the leading digits
	m := levelRe.FindStringSubmatch(rest)
	if m == nil || m[1] == "" {
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
//
// An unparseable field returns ErrUnparseable so the row routes to review
// instead of contributing a confident wrong number to a leaderboard.
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
//
// An unparseable field returns ErrUnparseable so the row routes to review
// instead of contributing a confident wrong number to a leaderboard.
func ParsePoints(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("ingest: points %q: %w", s, ErrUnparseable)
	}

	// Validate shape: either a plain number or comma-separated thousands
	if !pointsRe.MatchString(t) {
		return 0, fmt.Errorf("ingest: points %q: %w", s, ErrUnparseable)
	}

	// Strip commas and parse
	digits := strings.ReplaceAll(t, ",", "")
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ingest: points %q: %w", s, ErrUnparseable)
	}
	return n, nil
}
