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
	for _, in := range []string{"", "Power:", "Power: M", "banana"} {
		if _, err := ParsePower(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePower(%q) = %v, want ErrUnparseable", in, err)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]int{"Lv.35": 35, "Lv.4": 4, "35": 35} {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %d, want %d", in, got, want)
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
	for _, in := range []string{"", ",", "abc"} {
		if _, err := ParsePoints(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePoints(%q) = %v, want ErrUnparseable", in, err)
		}
	}
}
