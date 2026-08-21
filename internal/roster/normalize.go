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
	"unicode/utf8"

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

	// The ARCH, on the same stylised-substitution basis as Δ and with the
	// same caveat stated rather than hidden. Capture 1 carries a member whose
	// name ends in a tall fallback-font arch; tesseract reads the whole name
	// as "TYRION" on all eight sightings of it, and the roster fixture's own
	// note records the transcriber's reading as "the core reads TYRION" while
	// being explicit that the glyph "carries no diagonal and is therefore not
	// an N". So this is NOT a claim that the arch is drawn like an N. It is
	// the Δ claim: a player is using a non-Latin glyph as a letter of their
	// name, and the engine has no way to return anything but the Latin letter
	// it resembles, so the two forms would otherwise share no final character
	// and no threshold could reach them.
	//
	// Both candidate codepoints are folded because the transcription does not
	// settle between them -- "Armenian Ո and the set operator ∩ are both
	// consistent with it, and ∩ is a best reading rather than a settled
	// codepoint" -- and a fold that works only for the reading the transcriber
	// was unsure of is a fold that depends on a coin flip.
	//
	// The cost is the usual one and is measured, not argued: two members
	// differing only by n-versus-arch now collide, which is what
	// ClosestPairScore is the budget for. `make probe-roster` and both M4
	// gates print it.
	'∩': 'N', // U+2229 INTERSECTION
	'Ո': 'N', // U+0548 ARMENIAN CAPITAL LETTER VO

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

// stripDecoration removes leading and trailing runs of non-ASCII characters
// from an ALREADY-NORMALIZED string, provided an ASCII core survives.
//
// It exists because Normalize cannot drop these characters and should not try
// to. Normalize keeps letters and digits, and Unicode classifies the two
// ornaments this roster actually uses as exactly that: U+03DF GREEK SMALL
// LETTER KOPPA is Ll, and the Arabic-Indic digits in "٣١٢ A l i ٣١٢" are Nd.
// They survive normalization by the same rule that keeps any real character,
// and there is no property of the character itself that marks it as ornament.
// Position is the only signal available, which is why this works on runs at
// the ends rather than on a character class.
//
// Three properties carry the safety argument, and none of them is optional:
//
//   - It runs AFTER foldHomoglyph, on Normalize's output. A homoglyph is not
//     decoration: the player typed a character that RENDERS like a Latin one,
//     and folding turns it into that Latin one. "ΔKΔŽΔ" has already become
//     "akaza" by the time this sees it, so nothing is stripped. Strip first
//     instead and it reduces to "K".
//
//     Be precise about how that fails, because it is not how it looks: inside
//     TokenSetRatio the decoration score is taken as a MAXIMUM, so a strip
//     applied to raw text does not break a match -- it scores near zero,
//     loses the maximum, and contributes NOTHING while every other test still
//     passes. The failure is silent uselessness, not a regression, which is
//     the harder kind to notice. Only a pair whose sole route to a match runs
//     through folding AND stripping at once can detect it; that is what
//     TestDecorationStrippingSeesHomoglyphsAlreadyFolded is for.
//
//   - A string with NO ASCII core is returned untouched. "한씨아저씨" is a
//     name, not a decorated name, and there is nothing to strip it to.
//     Returning "" would be worse than useless: TokenSetRatio scores an empty
//     normalization as 0 against everything including another empty one, so
//     that member would stop matching even its own clean read.
//
//   - Only the ENDS are stripped, never the interior. A non-ASCII character
//     between two Latin runs is far likelier to be a homoglyph the fold table
//     does not yet carry than an ornament, and dropping it would silently
//     join two halves of a name that were never adjacent.
//
// The cost is separation: two members differing only in decoration collapse
// onto the same string. That is real and it is why ClosestPairScore is the
// budget this is read against, exactly as confusableCost is. On capture 6 the
// closest pair is unmoved at 60, against an AutoAccept of 92.
func stripDecoration(s string) string {
	r := []rune(s)
	core := func(c rune) bool { return c < utf8.RuneSelf }

	lo, hi := 0, len(r)
	for lo < hi && !core(r[lo]) {
		lo++
	}
	for hi > lo && !core(r[hi-1]) {
		hi--
	}
	if lo >= hi {
		return s
	}
	if lo == 0 && hi == len(r) {
		return s
	}
	return string(r[lo:hi])
}

// ornamentTokenMaxLen is how short an ASCII token has to be before it can be
// treated as what OCR made of an ornament. Two: the ornaments this roster
// carries are one or two glyphs ("狂", "ꙅઉ", "ϟϟ") and the engine returns one
// or two characters for them ("jt", "31", "9G", "96"). Three would start
// swallowing real name fragments -- "Angel 4am" and "Mar 89" both carry
// meaningful short tokens.
const ornamentTokenMaxLen = 2

// coreTokens drops the tokens of an already-token-normalized name that could
// be an ornament rather than part of the name: any non-ASCII token, and any
// ASCII token of at most ornamentTokenMaxLen characters. It returns the
// remainder joined with single spaces, or "" if nothing survives.
//
// It is the counterpart to stripDecoration for the case that function cannot
// see. stripDecoration works on Normalize's output, where whitespace has been
// collapsed away entirely, and it strips runs of NON-ASCII characters at the
// ends. That reaches "ϟϟ Leo ϟϟ" versus a read of ">> Lea >>", because the
// engine returned the ornament as punctuation and Normalize dropped it. It
// does NOT reach an ornament the engine returns as LETTERS OR DIGITS:
// "Danny 狂" reads as "Danny jt", "ZāP ꙅઉ" reads as "ZaP 96", and Normalize
// keeps "jt" and "96" for exactly the reason it keeps any letter or digit --
// there is no property of the character that marks it as ornament. Once the
// space is gone there is not even a boundary left to cut on. So this works on
// TOKENS, before the space is collapsed, and position is again the only
// signal available.
//
// THE SAFETY PROPERTY IS AT THE CALL SITE, not here. This is a licence for two
// names differing only in a short token to score alike, so TokenSetRatio
// applies it only when one of the two names actually carries a non-ASCII
// token -- i.e. only when comparing against a name that is decorated. A read
// of "Maso" against "Mar 89" never takes this path, because neither carries an
// ornament, and "89" is part of that member's name rather than something OCR
// made up. Without that guard this would be a general licence to drop short
// tokens from every comparison on the roster.
func coreTokens(tokens []string) string {
	var kept []string
	for _, t := range tokens {
		if !isASCII(t) || len([]rune(t)) <= ornamentTokenMaxLen {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, " ")
}

// hasNonASCIIToken reports whether any token is non-ASCII -- the condition
// under which coreTokens may be applied at all.
func hasNonASCIIToken(tokens []string) bool {
	for _, t := range tokens {
		if !isASCII(t) {
			return true
		}
	}
	return false
}

func isASCII(s string) bool {
	for _, r := range s {
		if r >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
