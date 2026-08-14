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
// true value of 218,700,000 -- wrong by 8.6x, and written as a confident
// fact). Removing the whitelist is half the fix; this asserts the other
// half, that ParsePower itself must not accept the laundered shape even if
// something upstream hands it one anyway (a whitelist "helpfully" re-added
// later, a different OCR path, a hand-crafted test fixture). Before this
// task's change, ParsePower("1877M") returned (1_877_000_000, nil) -- no
// error, so nothing routed this to review; see the task report for that
// exact pre-fix output.
func TestParsePowerRejectsTheWhitelistLaunderedShape(t *testing.T) {
	got, err := ParsePower("1877M")
	if err == nil {
		t.Fatalf("ParsePower(%q) = %d, <nil error> -- want ErrUnparseable; a K/M/B value with no decimal point is not how this UI renders power (every real row has exactly one decimal place) and must not become a confident fact", "1877M", got)
	}
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("ParsePower(%q) error = %v, want ErrUnparseable", "1877M", err)
	}
	if got == 1_877_000_000 {
		t.Errorf("ParsePower(%q) = %d: this is exactly the laundered wrong value (8.6x the true 218,700,000) the whitelist produced -- it must never be returned as if it were a fact", "1877M", got)
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
// misread, or a hand-built fixture. Every one of these is the exact shape
// task 23's own measurement produced by running the shipped powerSpec
// whitelist against 53 real member rows from capture 1: real values
// (168.1M, 210.6M, 164.1M, 21.75M, 1.1M, 190.1M -- see the task report for
// the full table) came back as "16841M", "2106M", "16441M", "2175M", "1M",
// "190.1M" once the whitelist was applied, some missing the decimal point
// entirely and some just a digit short of it. None of these may parse.
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
