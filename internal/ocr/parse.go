package ocr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrNotANumber reports text that ParseAbbrevNumber could not interpret.
var ErrNotANumber = errors.New("ocr: not a parseable number")

// ParseAbbrevNumber turns Last War's abbreviated numerals into an int64:
// "1234" → 1234, "500K" → 500000, "1.2M" → 1200000, "2B" → 2000000000.
// It is the cast behind the "power" field spec (design doc §3), where the game
// renders large values with K/M/B suffixes.
//
// TODO(you): implement this. It is a small but decision-rich parse — the
// choices are yours:
//   - Case: is "1.2m" as valid as "1.2M"? (the game is consistent, but OCR
//     is not always.)
//   - Rounding: "1.2345K" is 1234.5 — round, floor, or reject a fractional
//     result? Whatever you pick, be consistent and note it.
//   - Suffixes: K=1e3, M=1e6, B=1e9. A bare number has no suffix.
//   - Errors: empty string and non-numeric input must return ErrNotANumber
//     (wrap it with %w) rather than a zero value that reads as a real score.
//
// The tests in parse_test.go pin the contract; make them pass.
func ParseAbbrevNumber(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("ocr: empty input: %w", ErrNotANumber)
	}

	// A trailing K/M/B (case-insensitive) is a magnitude suffix.
	mult := 1.0
	switch strings.ToUpper(s[len(s)-1:]) {
	case "K":
		mult = 1e3
	case "M":
		mult = 1e6
	case "B":
		mult = 1e9
	}
	if mult != 1.0 {
		s = s[:len(s)-1]
	}

	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("ocr: %q is not a number: %w", s, ErrNotANumber)
	}
	// Round rather than truncate: a fractional OCR artifact should land on the
	// nearest whole value, not systematically shrink every score.
	return int64(math.Round(f * mult)), nil
}
