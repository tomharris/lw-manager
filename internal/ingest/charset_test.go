package ingest

import (
	"errors"
	"testing"
)

// This file tests the rule task 23 (see the module doc comments on
// powerSpec/levelSpec/lastActiveSpec in roster.go and vsPointsSpec in vs.go)
// put in code: a character whitelist is safe only where every character it
// removes would also have been absent from a correct read. Removing a
// character a correct read never contains cannot launder anything -- there is
// nothing legitimate left for it to strip -- so the property to test is not
// "does this whitelist happen to produce the right value on my sample" (that
// is what parse_test.go's value-level cases already do) but "is the charset a
// superset of what a correct read is built from at all". That property holds
// or fails independent of any particular OCR run, which is what makes it the
// thing worth asserting here rather than only in a measurement report.

// charsetSupersetOfSamples fails the test if any character in any sample
// string is missing from charset. samples should be strings a correct read
// of the field actually produces -- ParsePower/ParseLevel/etc.'s own accepted
// inputs are exactly that, since those are literally what the parser was
// built to recognize as correct.
func charsetSupersetOfSamples(t *testing.T, fieldName, charset string, samples []string) {
	t.Helper()
	allowed := map[rune]bool{}
	for _, r := range charset {
		allowed[r] = true
	}
	for _, s := range samples {
		for _, r := range s {
			if !allowed[r] {
				t.Errorf("%s charset %q is missing %q, which a correct read (%q) contains -- "+
					"the whitelist would strip a character a real read legitimately has, "+
					"which is exactly the shape of laundering Finding 7 found in powerSpec",
					fieldName, charset, string(r), s)
			}
		}
	}
}

// TestLevelCharsetCannotLaunderACorrectRead is the "why", not just the value,
// for keeping levelSpec's whitelist: every character a correct level read is
// built from -- "L", "v", ".", and digits -- is already in "Lv.0123456789",
// so the whitelist has no correct-read character available to strip. Task
// 23's own measurement (roster.go's levelSpec doc comment) found 0/53
// disagreements between whitelisted and unconstrained parses across real
// capture-1 rows, which is the value-level confirmation of this same
// structural property.
// The samples are deliberately restricted to strings this field's OCR could
// actually produce under the charset -- capital "L", lowercase "v", exactly
// as "Lv.0123456789" spells them -- not every spelling ParseLevel's own
// leniency accepts (it also takes "lv35", "LV.35", "Lv 35" for robustness
// against other input sources); those extra tolerances are not correct
// *reads of this charset-constrained field* and asserting the charset covers
// them would test the wrong thing.
func TestLevelCharsetCannotLaunderACorrectRead(t *testing.T) {
	samples := []string{"Lv.35", "Lv.4", "Lv34", "Lv4"}
	for _, s := range samples {
		if _, err := ParseLevel(s); err != nil {
			t.Fatalf("test fixture %q is not actually a correct ParseLevel read: %v", s, err)
		}
	}
	charsetSupersetOfSamples(t, "levelSpec", levelSpec.Charset, samples)
}

// TestLastActiveCharsetCannotLaunderACorrectRead: same property as the level
// test above, for lastActiveSpec. "0123456789hmdagoOnline " covers every
// character "Xh ago"/"Xm ago"/"Xd ago"/"Online" is built from, including both
// cases of "o" ("ago" and "Online" render it differently). Measured
// value-level effect (lastActiveSpec's doc comment in roster.go): 34/53 real
// rows parsed both with and without the whitelist, zero disagreements.
func TestLastActiveCharsetCannotLaunderACorrectRead(t *testing.T) {
	samples := []string{"Online", "online", "1h ago", "14h ago", "30m ago", "2d ago"}
	for _, s := range samples {
		if _, err := ParseLastActiveHours(s); err != nil {
			t.Fatalf("test fixture %q is not actually a correct ParseLastActiveHours read: %v", s, err)
		}
	}
	charsetSupersetOfSamples(t, "lastActiveSpec", lastActiveSpec.Charset, samples)
}

// TestVSPointsCharsetCannotLaunderACorrectRead: same property, for
// vsPointsSpec in vs.go. A correct VS points read is plain digits and commas
// ("101,286,241"), both already in "0123456789,". Measured value-level effect
// (vsPointsSpec's doc comment in vs.go): the whitelist recovered 14/15 real
// rows that failed unconstrained (a leading OCR artifact like "— " or "aoc "
// the whitelist correctly stripped), and every recovered digit sequence
// matched the row's real value -- none was laundered into a different one.
func TestVSPointsCharsetCannotLaunderACorrectRead(t *testing.T) {
	samples := []string{"45,048,150", "16,831,113", "0", "1,524,375", "101,286,241"}
	for _, s := range samples {
		if _, err := ParsePoints(s); err != nil {
			t.Fatalf("test fixture %q is not actually a correct ParsePoints read: %v", s, err)
		}
	}
	charsetSupersetOfSamples(t, "vsPointsSpec", vsPointsSpec.Charset, samples)
}

// TestPowerSpecHasNoCharset locks in task 23's fix against exactly the
// regression CLAUDE.md warns this class of finding invites: "It is the kind
// of thing that gets 'optimised' back in by someone who sees a whitelist as
// an obvious accuracy win." powerSpec cannot pass
// TestLevelCharsetCannotLaunderACorrectRead's style of property test --a
// correct read of "Power: 218.7M" contains "P", "o", "w", "e", "r", ":", " ",
// none of which any plausible power charset would include alongside
// "0123456789.KMB" -- so there is no safe charset for this field to fall
// back to, and this test asserts the field simply has none.
func TestPowerSpecHasNoCharset(t *testing.T) {
	if powerSpec.Charset != "" {
		t.Errorf("powerSpec.Charset = %q, want empty -- see this field's doc comment in roster.go "+
			"and docs/superpowers/specs/evidence/m4-ocr-2026-08-14 Finding 7 before restoring one: "+
			"a charset here was measured laundering 33/53 real rows into a wrong value", powerSpec.Charset)
	}
}

// TestParseLevelRejectsRealMisreadsFromBothConditions uses the actual raw
// OCR strings task 23's measurement produced from real capture-1 rows --
// both with and without levelSpec's charset applied -- to confirm noise from
// this field routes to review under either condition, not just in the
// synthetic cases parse_test.go already covers. These are frames where the
// crop did not contain a level at all (a "Manage" button edge, a blank gap),
// so both conditions correctly failing is the property being checked --
// unlike power, nothing here should ever parse.
func TestParseLevelRejectsRealMisreadsFromBothConditions(t *testing.T) {
	for _, in := range []string{
		"(WEE)", "(ES", "ee", "EE", "—", "EEE ———", "(EK.", "nn", "re",
		"", ".", "32", "34", "v2",
	} {
		if _, err := ParseLevel(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParseLevel(%q) did not route to review, want ErrUnparseable", in)
		}
	}
}

// TestParseLastActiveRejectsRealMisreadsFromBothConditions mirrors the level
// test above for lastActiveSpec, using raw text task 23's measurement pulled
// from real crops that were not actually a last-active field (a "Manage"
// button, a status-icon fragment) -- another field's content bleeding into
// this one must still fail to parse, whitelist or not.
func TestParseLastActiveRejectsRealMisreadsFromBothConditions(t *testing.T) {
	for _, in := range []string{
		"tH VY", "Op iS", "O i", "homer", "home", "1h ann",
		"| Manage |", "anage", "mh",
	} {
		if _, err := ParseLastActiveHours(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParseLastActiveHours(%q) did not route to review, want ErrUnparseable", in)
		}
	}
}
