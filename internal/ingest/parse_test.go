package ingest

import (
	"errors"
	"testing"
)

func TestParsePower(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"Power: 216.2M", 216_200_000},
		{"216.2M", 216_200_000},
		{"Power: 1.5B", 1_500_000_000},
		{"Power: 987.6K", 987_600},
		{"Power: 232.2M", 232_200_000},
		// A real member row's true value, reproduced from
		// docs/superpowers/specs/evidence/m4-ocr-2026-08-14 Finding 7 and
		// re-confirmed against a real capture-1 frame (task 23's report):
		// a correct read still parses to the true value once the charset
		// whitelist that used to corrupt it is gone.
		{"Power: 218.7M", 218_700_000},
	}
	for _, c := range cases {
		got, err := ParsePower(c.in)
		if err != nil {
			t.Errorf("ParsePower(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePower(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestParsePowerRejectsTheWhitelistLaunderedShape is the regression for task
// 23's finding: docs/superpowers/specs/evidence/m4-ocr-2026-08-14/README.md
// Finding 7 measured that powerSpec's charset whitelist turns a real member
// row's raw OCR text "Power:je18°7M" (which correctly fails ParsePower's
// regex today) into "1877M" (which parsed cleanly to 1,877,000,000 against a
// true value of 218,700,000 -- wrong by 8.6x). This did not reach a
// leaderboard -- every one of the 53 laundered reads task 23 measured scored
// OCR confidence 0.00, which factConfidenceGate would have queued for review
// rather than written as a fact (see ParsePower's own doc comment for the
// fix-round correction on this point) -- but ParsePower calling a laundered
// string well-formed at all is still the bug: it means a human reviewer sees
// the plausible "1877M" instead of the visibly-broken original, and nothing
// downstream would catch a wrong value if this field's OCR confidence ever
// improves. Removing the whitelist is half the fix; this asserts the other
// half, that ParsePower itself must not accept the laundered shape even if
// something upstream hands it one anyway (a whitelist "helpfully" re-added
// later, a different OCR path, a hand-crafted test fixture). Before this
// task's change, ParsePower("1877M") returned (1_877_000_000, nil) -- no
// error, so nothing routed this to review; see the task report for that
// exact pre-fix output.
func TestParsePowerRejectsTheWhitelistLaunderedShape(t *testing.T) {
	got, err := ParsePower("1877M")
	if err == nil {
		t.Fatalf("ParsePower(%q) = %d, <nil error> -- want ErrUnparseable; a K/M/B value with no decimal point is not how this UI renders power (every real row has exactly one decimal place) and must not parse as well-formed", "1877M", got)
	}
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("ParsePower(%q) error = %v, want ErrUnparseable", "1877M", err)
	}
	if got == 1_877_000_000 {
		t.Errorf("ParsePower(%q) = %d: this is exactly the laundered wrong value (8.6x the true 218,700,000) the whitelist produced -- it must never be returned as if it were well-formed", "1877M", got)
	}
}

func TestParsePowerRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"",            // empty
		"Power:",      // no value
		"Power: M",    // no numeric part
		"banana",      // no suffix
		"216. 2M",     // interior space (malformed)
		"216.2M junk", // trailing characters
		"2162M",       // no decimal point -- see TestParsePowerRejectsDecimalLessValues
	} {
		if _, err := ParsePower(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePower(%q) = %v, want ErrUnparseable", in, err)
		}
	}
}

// TestParsePowerRejectsDecimalLessValues is requirement 4 of task 23: every
// power value on both screens renders with exactly one decimal place, so a
// K/M/B-suffixed value with none is structurally suspect regardless of how
// it was produced -- a charset whitelist (Finding 7, and
// TestParsePowerRejectsTheWhitelistLaunderedShape above), a different OCR
// misread, or a hand-built fixture. Every one of these is the exact string
// task 23's own measurement produced by running the shipped powerSpec
// whitelist against 53 real member rows from capture 1; five were checked
// directly against their source crop (fix-round correction: an earlier draft
// of this comment mis-stated two of the true values, caught in review) --
// true 168.1M read back as "16841M", true 210.6M as "2106M", true 164.1M as
// "16441M", true 217.5M as "2175M" (a dropped decimal point each time), and
// true 228.1M as just "1M" (the whitelist kept only the trailing digit and
// the suffix). None of these may parse.
func TestParsePowerRejectsDecimalLessValues(t *testing.T) {
	for _, in := range []string{
		"16841M", "2106M", "16441M", "22951M", "1889M", "2175M", "1863M",
		"115M", "31892M", "1950M", "81M", "1936M", "1M", "1843M", "2215M",
		"1667M", "2187M", "1850M", "1736M", "2419M",
	} {
		if _, err := ParsePower(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePower(%q) did not reject a decimal-less K/M/B value, want ErrUnparseable", in)
		}
	}
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"Lv.35", 35},
		{"Lv.4", 4},
		{"Lv 35", 35},
		{"lv.35", 35},
		{"LV.35", 35},
		{"lv35", 35},
	}
	for _, c := range cases {
		got, err := ParseLevel(c.in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLevel(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseLevelRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"35",                  // bare number without Lv prefix
		"Power: 216.2M Lv.35", // grabs power digits instead of level
	} {
		if _, err := ParseLevel(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParseLevel(%q) = %v, want ErrUnparseable", in, err)
		}
	}
}

func TestParseLastActiveHours(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"Online", 0},
		{"online", 0},
		{"1h ago", 1},
		{"14h ago", 14},
		{"30m ago", 0.5},
		{"2d ago", 48},
	}
	for _, c := range cases {
		got, err := ParseLastActiveHours(c.in)
		if err != nil {
			t.Errorf("ParseLastActiveHours(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLastActiveHours(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseLastActiveHoursRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"Power: 216.2M", // lowercase m suffix matches as minutes
		"216.2M",        // same issue: 2m ago
		"Lv.35",         // unanchored regex would match "5"
	} {
		if _, err := ParseLastActiveHours(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParseLastActiveHours(%q) = %v, want ErrUnparseable", in, err)
		}
	}
}

func TestParsePoints(t *testing.T) {
	for in, want := range map[string]int64{
		"45,048,150": 45_048_150,
		"16,831,113": 16_831_113,
		"0":          0,
		"1,524,375":  1_524_375,
	} {
		got, err := ParsePoints(in)
		if err != nil {
			t.Errorf("ParsePoints(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePoints(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParsePointsRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"",              // empty
		",",             // just comma
		"abc",           // non-numeric
		"45,048,150 #3", // rank badge concatenated
		"45.048.150",    // periods instead of commas
		"-45",           // negative sign
	} {
		if _, err := ParsePoints(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePoints(%q) = %v, want ErrUnparseable", in, err)
		}
	}
}

// TestParseGroupHeaderToleratesBadgeAndChevronNoise is task 24's test 3: the
// real review-queue text task 24's brief quotes verbatim (badge shield
// outline surviving as "{", ")"; chevron button surviving as "yi]") must
// still yield the count, even though the old anchored pattern
// (^(R\d+)\s+.+\s(\d+)/(\d+)$) would have rejected the trailing "yi]" as
// text after the final anchor.
func TestParseGroupHeaderToleratesBadgeAndChevronNoise(t *testing.T) {
	name, total, err := parseGroupHeader("{R3) Footloose 10/64 yi]")
	if err != nil {
		t.Fatalf("parseGroupHeader: %v", err)
	}
	if total != 64 {
		t.Errorf("total = %d, want 64", total)
	}
	if name == "" {
		t.Error("name is empty, want the noisy-but-present group name kept for review triage")
	}
}

// TestParseGroupHeaderRejectsTwoCountTokens is test 4: two "N/M"-shaped
// tokens in one line have no principled way to choose between them, so both
// must be refused rather than one silently picked (parseGroupHeader's own
// doc comment).
func TestParseGroupHeaderRejectsTwoCountTokens(t *testing.T) {
	if _, _, err := parseGroupHeader("R3) Footloose 10/64 3/9"); !errors.Is(err, ErrUnparseable) {
		t.Errorf("parseGroupHeader with two count tokens = %v, want ErrUnparseable", err)
	}
}

// TestParseGroupHeaderRejectsMissingCount is test 5: task 24's brief quotes
// this exact real OCR text ("[R4) This Is It ap") as a line with zero
// count-shaped tokens, which must still fail to review rather than guess.
func TestParseGroupHeaderRejectsMissingCount(t *testing.T) {
	if _, _, err := parseGroupHeader("[R4) This Is It ap"); !errors.Is(err, ErrUnparseable) {
		t.Errorf("parseGroupHeader with no count token = %v, want ErrUnparseable", err)
	}
}

// TestParseGroupHeaderRejectsEmpty guards the zero-token path at its other
// edge -- an empty or whitespace-only header must not be treated as a
// single implicit match.
func TestParseGroupHeaderRejectsEmpty(t *testing.T) {
	if _, _, err := parseGroupHeader(""); !errors.Is(err, ErrUnparseable) {
		t.Errorf("parseGroupHeader(\"\") = %v, want ErrUnparseable", err)
	}
}

// TestParseGroupHeaderRejectsAnIncoherentCount pins the structural coherence
// check, which the unanchored pattern needs and shipped without: a token can
// be unique, count-shaped and confident-looking while still being a truncation
// of the real one. "10/6 4]" is not contrived -- a detached trailing digit is
// the chevron-noise shape the evidence records, and the old anchored pattern
// rejected it only incidentally, because "4]" sat past its "$".
//
// The first case is the one that matters. total feeds groupTracker.expected,
// which gates member creation (roster.go), so a fabricated 6 against a real
// 64-member group would stop the other 58 from ever being created. See
// parseGroupHeader's doc comment for why N <= M closes that half and why no
// upper bound on M is invented to chase the other.
func TestParseGroupHeaderRejectsAnIncoherentCount(t *testing.T) {
	for _, raw := range []string{
		"{R3) Footloose 10/6 4]", // truncated: N > M
		"{R3) Footloose 0/0 yi]", // no group can have a zero total
		"R3 Footloose 5/0",       // denominator zero with a plausible numerator
	} {
		if _, _, err := parseGroupHeader(raw); !errors.Is(err, ErrUnparseable) {
			t.Errorf("parseGroupHeader(%q) = %v, want ErrUnparseable", raw, err)
		}
	}
}

// TestParseGroupHeaderAcceptsEveryRealHeader keeps the coherence check above
// honest in the other direction. These three are the only group headers read
// off real frames (docs/superpowers/specs/evidence/m4-ocr-2026-08-14), and a
// rule that rejected any of them would be worse than the fabrication it
// prevents. N == M is included deliberately: a full group is the boundary the
// "N <= M" check must not exclude.
func TestParseGroupHeaderAcceptsEveryRealHeader(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"{R2) I'm Alright 1/11", 11},
		{"{R3) Footloose 10/64 yi]", 64},
		{"(R4) This Is It 2/9 ap", 9},
		{"{R1) Full Group 9/9", 9},
	} {
		_, total, err := parseGroupHeader(tc.raw)
		if err != nil {
			t.Errorf("parseGroupHeader(%q) = %v, want no error", tc.raw, err)
			continue
		}
		if total != tc.want {
			t.Errorf("parseGroupHeader(%q) total = %d, want %d", tc.raw, total, tc.want)
		}
	}
}

// TestParseGroupHeaderRefusesChevronBleed is the chevron-bleed corpus: every
// distinct string groupHeaderRegion actually handed parseGroupHeader on the 22
// frames of capture 1 that it dropped, taken verbatim from
// `make probe-roster PROBE_ARGS=-roster.header` (and from the review_queue rows
// the same reads produced on the 2026-08-19 re-ingest). Every one keeps the
// group name and loses the N/M count, because the region's right edge used to
// sit at X2=0.97 -- x=698 of a 720px frame -- with the collapse chevron
// (x=651..683) entirely inside it.
//
// They are pinned as REFUSALS, not as things to be parsed, and this test must
// go on passing now that the crop has moved: the strings are what a chevron
// inside the crop produces, and the fix is that the crop no longer contains
// one. Relaxing the parser to accept them would be the wrong fix and an
// actively dangerous one -- task 24's review showed a fabricated count of 6
// against a real 64-member group stops the other 58 members being created at
// all (see parseGroupHeader's doc comment on N <= M).
//
// It is untagged deliberately, so real chevron bleed survives in `make test`
// with no device, no Docker, no blob store and no tesseract.
// Two tests already pin the other half of the same rule, which is why no third
// one is added here: TestParseGroupHeaderAcceptsEveryRealHeader immediately
// above parses "{R2) I'm Alright 1/11" and "{R3) Footloose 10/64 yi]", and
// TestParseGroupHeaderToleratesBadgeAndChevronNoise, five tests above, parses
// the same chevron noise this corpus is made of. So what is pinned here is the
// absence of a count, not a blanket refusal of anything the chevron touched.
func TestParseGroupHeaderRefusesChevronBleed(t *testing.T) {
	for _, raw := range []string{
		"R2) I'm Alright VN iy]",  // x8 of the 22
		"iR2) I'm Alright VN iy]", // x4
		"iR2) I'm Alright Vn WY",  // x3
		"R2) I'm Alright VN iv]",  // x2
		"R2) I'm Alright VN iy}",
		"R2) I'm Alright VW iv]",
		"R2) I'm Alright Vn WY",
		"R2) I'm Alright Vu iy]",
		"[R4) This Is It ap", // the one R4 frame; "2/9" gone entirely
	} {
		if _, _, err := parseGroupHeader(raw); !errors.Is(err, ErrUnparseable) {
			t.Errorf("parseGroupHeader(%q) accepted a header with no count; want ErrUnparseable", raw)
		}
	}
}
