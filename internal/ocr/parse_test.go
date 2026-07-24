package ocr

import "testing"

func TestParseAbbrevNumber(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1234", 1234, false},
		{"500K", 500000, false},
		{"1.2M", 1200000, false},
		{"2B", 2000000000, false},
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := ParseAbbrevNumber(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseAbbrevNumber(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAbbrevNumber(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseAbbrevNumber(%q): got %d, want %d", c.in, got, c.want)
		}
	}
}
