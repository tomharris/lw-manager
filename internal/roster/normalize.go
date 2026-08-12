// Package roster matches an OCR-read name to a known alliance member.
//
// Last War names are full of unicode decoration, letter spacing, homoglyphs
// and alliance tags, and OCR adds its own noise on top. Normalization does
// most of the work here; the fuzzy score only has to cover what survives it.
//
// The matcher is hand-rolled rather than vendored, consistent with the
// hand-rolled NCC in internal/vision and with CGO_ENABLED=0.
package roster

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Normalize reduces a displayed name to a comparable form: compatibility
// decomposition, combining marks removed, non-alphanumerics dropped, and all
// whitespace collapsed away, casefolded.
//
// Collapsing internal whitespace entirely is the highest-value step and is
// deliberate rather than incidental: the member list renders some names
// letter-spaced ("M I C H E L L"), and OCR faithfully reports the spaces.
// Removing them turns an unmatchable string into an exact match with no fuzzy
// scoring involved at all.
func Normalize(s string) string {
	d := norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(d))
	for _, r := range d {
		switch {
		case unicode.Is(unicode.Mn, r):
			// A combining mark: drop it, so "é" has already become "e" + mark
			// and we keep only the "e".
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			// Spaces, punctuation, emoji and decorations are all dropped.
		}
	}
	return b.String()
}

// NormalizeTokens is Normalize but preserving word boundaries, for the token
// set ratio. Decoration is still stripped; only runs of whitespace survive, as
// single spaces.
func NormalizeTokens(s string) string {
	d := norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(d))
	prevSpace := false
	for _, r := range d {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevSpace = false
		case unicode.IsSpace(r):
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
