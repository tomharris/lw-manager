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
		{"2162M", 2_162_000_000}, // no decimal, legitimately shaped
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

func TestParsePowerRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"",            // empty
		"Power:",      // no value
		"Power: M",    // no numeric part
		"banana",      // no suffix
		"216. 2M",     // interior space (malformed)
		"216.2M junk", // trailing characters
	} {
		if _, err := ParsePower(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePower(%q) = %v, want ErrUnparseable", in, err)
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
