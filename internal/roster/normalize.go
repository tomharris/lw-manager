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
		r = foldHomoglyph(r)
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

// foldHomoglyph maps a character that renders identically to a Latin one onto
// that Latin character. NFKD cannot do this job: a Cyrillic "о" and a Latin
// "o" are different letters that happen to be drawn the same, and no
// decomposition relates them.
//
// It matters because OCR reads the glyph, not the codepoint. It has no way to
// return the Cyrillic "о" a player typed, and never will — so a name carrying
// one could not be matched at any threshold, since the stored and the read
// form share no characters to score against. The first real M4 gate run
// measured that: rank 1 renders "ΔKΔŽΔ", OCR read "AKAZA", and the normalized
// forms were "δkδzδ" and "akaza".
//
// The table is deliberately confined to characters that are visually
// identical (or, for Δ, conventionally used as the Latin letter it resembles
// and read as one). A script with its own distinct shapes — Korean, CJK,
// Arabic — is never folded: those decorate a name rather than impersonate a
// Latin character, and collapsing them would let two unrelated members
// normalize onto one key, writing one member's score onto another's row.
// Losing a row to the review queue is recoverable; that is not.
func foldHomoglyph(r rune) rune {
	if r < 0x0080 {
		return r // ASCII fast path: nothing to fold, and most input is this
	}
	if f, ok := homoglyphs[r]; ok {
		return f
	}
	return r
}

var homoglyphs = map[rune]rune{
	// Cyrillic. The classic confusable set — each of these is drawn exactly
	// as its Latin counterpart in every font this game uses.
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H',
	'О': 'O', 'Р': 'P', 'С': 'C', 'Т': 'T', 'У': 'Y', 'Х': 'X',
	'Ѕ': 'S', 'І': 'I', 'Ј': 'J', 'Ү': 'Y',
	'а': 'a', 'в': 'b', 'е': 'e', 'к': 'k', 'м': 'm', 'н': 'h',
	'о': 'o', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x',
	'ѕ': 's', 'і': 'i', 'ј': 'j',

	// Greek. Uppercase and lowercase are listed separately because they do
	// not fold to the same Latin letter: uppercase Ν is an N, but lowercase
	// ν is drawn as a v.
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H', 'Ι': 'I',
	'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O', 'Ρ': 'P', 'Τ': 'T',
	'Υ': 'Y', 'Χ': 'X',
	'α': 'a', 'β': 'b', 'ε': 'e', 'ι': 'i', 'κ': 'k', 'ν': 'v',
	'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x', 'γ': 'y',
	// Δ is not a lookalike in the strict sense — it is a triangle, not an A.
	// It is folded anyway because this game's players use it as a stylised A
	// and OCR reads it as one; capture 6's rank 1 is the measured case.
	'Δ': 'A',

	// Latin letters NFKD leaves alone, because the stroke through them is
	// part of the letter rather than a combining mark.
	'ł': 'l', 'Ł': 'L', 'ø': 'o', 'Ø': 'O', 'đ': 'd', 'Đ': 'D',
	'ı': 'i', 'İ': 'I', 'ſ': 's',
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
		// Folded here too, and for the same reason Normalize folds: the two
		// feed different scorers over the same pair of names, and a
		// homoglyph that survived only one of them would make the token set
		// ratio and the edit distance disagree about whether two strings
		// share a character.
		r = foldHomoglyph(r)
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
