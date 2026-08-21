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
// exactly the shape task 23's audit found dangerous. Every power value
// *measured* is rendered with exactly one decimal place -- 212.1M, 290.0M,
// 218.7M, 155.7M -- across every real row task 23 sampled (53 rows, capture
// 1) and every example in the evidence corpus, but that sample is narrower
// than the field: every row measured falls in a single 164M-290M band, all
// M-suffixed (no K, no B), and R1 -- 12 members, the alliance's own smallest
// group and the one likeliest to hold a K-suffixed or sub-100M value --
// never appears in capture 1 at all. ParsePower's own doc comment ("at most
// four significant figures") implies a value below 100M would legitimately
// carry *two* decimals (21.75M), which this regex would reject. So this is a
// premise bounded by what has been measured, not a rule proven for the whole
// field: today's quantified loss from it is 0 of 53 real rows, the failure
// direction is safe (ErrUnparseable routes to review, never a silent drop),
// and a capture that finally includes R1 -- or any K/B-suffixed row -- is
// the test that would actually settle whether two-decimal sub-100M values
// exist on this UI. A decimal-less K/M/B value is not a rarer-but-valid
// render *within the measured band*: it was either misread outright, or
// (before this task) produced by a charset whitelist that recognized digits
// where the true pixels were the decimal point and a following digit, off
// by roughly 10x. See ParsePower's own doc comment for that measurement and
// for how much protection this regex actually buys on its own (less than
// "independent guard" implies -- see the note there), and
// TestParsePowerRejectsTheWhitelistLaunderedShape for the regression it
// closes.
var (
	powerRe = regexp.MustCompile(`^[0-9]+\.[0-9][KMB]$`)
	levelRe = regexp.MustCompile(`^(?:lv\.?|lv\s)?\s*([0-9]+)\s*$`)
	agoRe   = regexp.MustCompile(`^([0-9]+)\s*([hmd])\s*(ago)?$`)

	// pointsRe requires comma grouping for anything longer than three digits
	// -- it used to also accept a bare, ungrouped digit run of any length
	// (`|[0-9]+`), which is exactly the shape vsPointsSpec's former charset
	// whitelist laundered garbage into (task 23 fix-round finding C1): a
	// real crop reading "7c 3240 7604" unconstrained -- correctly failing to
	// parse -- became "3732407604" once the whitelist forced tesseract's
	// classifier to reclassify every glyph as a digit, and the old bare
	// branch accepted a 10-digit run with no comma exactly like that without
	// complaint. Every real VS points value on screen is comma-grouped
	// (Finding 3: 101,286,241; this task's own measurement: 92,334,341 and
	// others), so an ungrouped run of more than three digits is exactly as
	// suspicious here as a decimal-less power value is in powerRe above.
	// This is not a complete guard: a garbage read that happens to collapse
	// to three or fewer digits (a bare "7", say) still satisfies the first
	// group alone, the same shape of gap powerRe's decimal check has against
	// "36.0M" (see ParsePower's doc comment) -- but it closes every
	// multi-group fabrication the review measured. See ParsePoints' own doc
	// comment and vsPointsSpec in vs.go for why the whitelist was removed
	// rather than kept alongside this.
	pointsRe = regexp.MustCompile(`^[0-9]{1,3}(?:,[0-9]{3})*$`)

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
// wrong by 8.6x. (Finding 7 called this "a confident wrong fact"; it was
// not, quite -- see the fix-round correction below.) Constraining
// tesseract's classifier to a charset does not just filter the text it
// already recognized; it changes which glyph each blob is classified as,
// and the decimal point -- a single small blob -- was the first casualty on
// 33 of 53 real rows measured, even though "." is itself in that charset.
// Removing the whitelist alone fixed 0/53 false accepts in that same
// measurement (every unconstrained read either parsed correctly or failed
// this regex), which is why the whitelist is gone from powerSpec entirely
// (see roster.go) rather than merely narrowed.
//
// Impact, corrected: every one of those 33 laundered reads scored OCR
// confidence 0.00 under the shipped Options (task 23's fix-round
// re-measurement), so factConfidenceGate would have queued all of them as
// low_confidence_power rather than writing a fact -- participation_facts
// held zero power rows before this fix, and none of the 33 reached a
// leaderboard. The defect was real regardless: ParsePower called a
// laundered string well-formed, so a human landed on "1877M" in the review
// queue rather than the visibly-broken "Power:je18°7M" it actually came
// from, and a future confidence improvement on this field's OCR would have
// had nothing left to catch a wrong value once it started scoring above
// 0.80. Neither of those is "corrupted the facts table"; both are still
// worth fixing, which is why the regex is tightened regardless of what
// currently gates it.
//
// This regex is a second guard, not an independent one: measured with the
// whitelist hypothetically restored, 3 of 53 whitelisted reads still
// satisfy this tightened pattern, and one of them is wrong --
// `frame_50_power_row4` reads "36.0M" against a true 236.0M (a lost leading
// digit, task 21's own ground truth), 6.6x off in a well-formed,
// decimal-bearing string this regex cannot distinguish from a correct read.
// So against a re-added whitelist this catches 30 of 33, not 33 of 33: the
// whitelist's removal is doing the real work, and this check is
// defense-in-depth against a decimal-less value reaching ParsePower by some
// other route (a different OCR path, a whitelist "helpfully" re-added
// later, a hand-built fixture) -- worth having, not sufficient on its own,
// because CLAUDE.md invariant #5 does not get to depend on which caller
// produced the string.
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
//
// pointsRe requires comma grouping, not a bare digit run of any length --
// task 23's fix-round review (finding C1) measured vsPointsSpec's former
// charset whitelist ("0123456789,") manufacturing a parseable number out of
// crops that were not a points read at all, on 6 of 11 real bands cut from a
// committed VS frame: a crop straddling two rows, reading "7c 3240 7604"
// unconstrained (correctly failing to parse), became "3732407604" once the
// whitelist forced every glyph into a digit. The old bare-`[0-9]+` branch
// accepted that outright. The whitelist is gone (see vsPointsSpec in vs.go
// for why "the charset is a superset of a correct read" was not, by itself,
// a safe argument here), and this regex is the same defense-in-depth
// powerRe's decimal requirement is for power: not sufficient alone -- a
// garbage read that happens to collapse to three or fewer digits still
// satisfies the ungrouped first group -- but it closes every multi-group
// fabrication measured, including the largest one (249,594,593,473 from
// "24959 n459 3473").
func ParsePoints(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("ingest: points %q: %w", s, ErrUnparseable)
	}

	// Validate shape: comma-grouped digits only -- see pointsRe's own doc
	// comment for why a bare, ungrouped run is no longer accepted.
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
// as "yi]", "ap", "VN iy]" — real review-queue text, task 24's brief). Two
// "N/M" tokens in one line (a misread word that happens to contain one, or
// two genuine counts from a merged line) have no principled way to choose
// between them, so this refuses both rather than picking one arbitrarily.
//
// Position-independence alone is not enough to stop a fabrication, and the
// first version of this relaxation shipped believing it was. Dropping the end
// anchor let a *truncated* token through: "{R3) Footloose 10/6 4]" has one
// unique count-shaped token, "10/6", because the tail digit detached into the
// chevron noise — and a detached trailing digit is exactly the noise shape the
// evidence records ("(es Thisisit CED 4]", docs/superpowers/specs/evidence/
// m4-ocr-2026-08-14). The old anchored pattern rejected that string only
// incidentally, because "4]" sat past its "$". So the token must now also be
// structurally coherent: M > 0, and N <= M, which no real header can violate
// (the three read off real frames are 2/9, 1/11 and 10/64) and which "10/6"
// and "0/0" both do.
//
// That is a shape check, not a tuned threshold, and it is deliberately not a
// bound on how large M may be: group sizes are user-controlled and the group
// set varies (CLAUDE.md, "Rank groups have no fixed identity"), so any cap
// would be a number invented here that a real alliance could one day exceed.
// The residual is stated rather than hidden, in the same spirit as ParsePoints'
// own doc comment: a detached digit joining the *denominator* ("10/641" from a
// true 10/64) is coherent by this rule and still gets through. Its direction of
// harm is the survivable one — total feeds groupTracker.expected, and an
// inflated expected leaves the group short of its own header, which
// IngestRoster reports as status "partial". An under-count is the one that does
// silent damage, because expected also gates member creation
// (groupTracker.canCreate in roster.go): a fabricated 6 against a
// real 64-member group stops the other 58 from being created at all and queues
// them as no_confident_match_group_full. N <= M is precisely the rule that
// closes the under-counting half.
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
	shown, shownErr := strconv.Atoi(parts[0])
	total, convErr := strconv.Atoi(parts[1])
	if shownErr != nil || convErr != nil {
		// Unreachable given groupHeaderCountRe's own \d+ shape, but a
		// strconv failure must still route to review rather than panic --
		// the same defensive shape every other Parse* function in this file
		// takes on its own regex-validated capture.
		return "", 0, fmt.Errorf("ingest: group header %q: %w", raw, ErrUnparseable)
	}
	if total <= 0 || shown > total {
		return "", 0, fmt.Errorf("ingest: group header %q: count %q is not a coherent N/M: %w", raw, token, ErrUnparseable)
	}
	return strings.TrimSpace(trimmed[:loc[0]]), total, nil
}
