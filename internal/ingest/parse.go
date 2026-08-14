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

// powerRe requires the decimal point, deliberately, where the shape a naive
// reading of the UI suggests ("208M" looks like a fine abbreviation) is
// exactly the shape task 23's audit found dangerous. Every power value on
// both screens is rendered with exactly one decimal place -- 212.1M, 290.0M,
// 218.7M, 155.7M, confirmed across every real row measured, both in the
// evidence corpus and in task 23's own re-measurement of 53 real member rows
// from capture 1 (docs/superpowers/specs/evidence/m4-ocr-2026-08-14). A
// decimal-less K/M/B value is therefore not a rarer-but-valid render, it is
// a corrupted one: it was either misread outright, or (before this task)
// produced by a charset whitelist that recognized digits where the true
// pixels were the decimal point and a following digit, off by roughly 10x.
// Making the decimal mandatory turns that shape back into ErrUnparseable --
// routed to review, per invariant #5 -- rather than a plausible-looking
// fact. See ParsePower's own doc comment for the measurement that justified
// this, and TestParsePowerRejectsTheWhitelistLaunderedShape for the
// regression it closes.
var (
	powerRe  = regexp.MustCompile(`^[0-9]+\.[0-9][KMB]$`)
	levelRe  = regexp.MustCompile(`^(?:lv\.?|lv\s)?\s*([0-9]+)\s*$`)
	agoRe    = regexp.MustCompile(`^([0-9]+)\s*([hmd])\s*(ago)?$`)
	pointsRe = regexp.MustCompile(`^(?:[0-9]{1,3}(?:,[0-9]{3})*|[0-9]+)$`)

	// groupHeaderCountRe finds an "N/M" token anywhere in a sticky group
	// header's raw OCR text -- unanchored, deliberately, unlike every other
	// regex in this file. See parseGroupHeader's own doc comment for why.
	groupHeaderCountRe = regexp.MustCompile(`\d+/\d+`)
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
//
// powerRe requires exactly one digit after the decimal point, not merely a
// decimal point somewhere: task 23's audit (docs/superpowers/specs/evidence/
// m4-ocr-2026-08-14 Finding 7, and the task's own report) found that
// powerSpec's former charset whitelist ("0123456789.KMB") laundered a real
// row's raw text -- "Power: 218.7M", which OCRs unconstrained as something
// like "Power:je18°7M" and correctly fails this regex -- into "1877M",
// which parsed cleanly to 1,877,000,000 against a true value of 218,700,000:
// wrong by 8.6x, with nothing downstream able to tell it from a good read.
// Constraining tesseract's classifier to a charset does not just filter the
// text it already recognized; it changes which glyph each blob is
// classified as, and the decimal point -- a single small blob -- was the
// first casualty on 33 of 53 real rows measured, even though "." is itself
// in that charset. Removing the whitelist alone fixed 0/53 false accepts in
// that same measurement (every unconstrained read either parsed correctly or
// failed this regex), which is why the whitelist is gone from powerSpec
// entirely (see roster.go) rather than merely narrowed. This regex is the
// second, independent guard: even a decimal-less value that reaches
// ParsePower by some other route (a different OCR path, a whitelist
// "helpfully" re-added later, a hand-built fixture) must still fail rather
// than silently parse, because CLAUDE.md invariant #5 does not get to
// depend on which caller produced the string.
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

// parseGroupHeader extracts a rank group's display name and its stated total
// from a sticky header's raw OCR text, e.g. "{R3) Footloose 10/64 yi]" ->
// ("{R3) Footloose", 64).
//
// It used to also extract the rank badge itself ("R3"), anchored at both
// ends of the line (`^(R\d+)\s+.+\s(\d+)/(\d+)$`) — task 24's brief: "those
// anchors were added deliberately after earlier parsers fabricated
// confident wrong numbers, and a previous implementer correctly declined to
// loosen them as a band-aid." That constraint has not been loosened here;
// it has been made unnecessary. Rank now comes from matchRankBadge's NCC
// read (rankbadge.go), never from OCR of the badge's outlined glyphs — see
// CLAUDE.md's note on outlined game glyphs, and Finding 4 (docs/superpowers/
// specs/evidence/m4-ocr-2026-08-14) for the measurement that ruled OCR out
// for that shape entirely, under every PSM and charset tried. With rank
// supplied elsewhere, the only claim this function still has to defend is
// the count, and it defends it the same way the old anchors did: refuse
// rather than guess.
//
// The pattern is anchored on shape, not on position. It requires exactly one
// "N/M"-shaped token anywhere in the line and fails if there are zero or
// more than one, which tolerates arbitrary leading and trailing OCR noise
// (the shield outline reading as "{", "(", ")"; the chevron button reading
// as "yi]", "ap", "VN iy]" — real review-queue text, task 24's brief) without
// ever being able to fabricate a count: two "N/M" tokens in one line (a
// misread word that happens to contain one, or two genuine counts from a
// merged line) have no principled way to choose between them, so this
// refuses both rather than picking one arbitrarily.
//
// name is whatever text precedes the matched count, trimmed — kept for
// review triage (a human looking at a queued row benefits from seeing
// "Footloose" even wrapped in "{R3) Footloose") but never used as a key:
// group names are user-editable and the group set itself varies (CLAUDE.md,
// "Rank groups have no fixed identity"), so every group-keyed structure in
// this package (GroupTally, groupTracker, run.groups) is keyed on the rank
// matchRankBadge supplies, not on this string.
func parseGroupHeader(raw string) (name string, total int, err error) {
	trimmed := strings.TrimSpace(raw)
	locs := groupHeaderCountRe.FindAllStringIndex(trimmed, -1)
	if len(locs) != 1 {
		return "", 0, fmt.Errorf("ingest: group header %q: found %d count-shaped tokens, want exactly 1: %w", raw, len(locs), ErrUnparseable)
	}
	loc := locs[0]
	token := trimmed[loc[0]:loc[1]]
	parts := strings.SplitN(token, "/", 2)
	total, convErr := strconv.Atoi(parts[1])
	if convErr != nil {
		// Unreachable given groupHeaderCountRe's own \d+ shape, but a
		// strconv failure must still route to review rather than panic --
		// the same defensive shape every other Parse* function in this file
		// takes on its own regex-validated capture.
		return "", 0, fmt.Errorf("ingest: group header %q: %w", raw, ErrUnparseable)
	}
	return strings.TrimSpace(trimmed[:loc[0]]), total, nil
}
