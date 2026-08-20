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
//
// The fix-round review (finding C1) sharpened this rule once: the superset
// property is necessary but not sufficient, because it only reasons about
// the *text* a correct read produces, not about what else the *crop* can
// contain. vsPointsSpec's charset ("0123456789,") was a superset of every
// character "101,286,241" is built from, exactly like levelSpec and
// lastActiveSpec below -- and it still laundered, because its crop sits
// against the alliance-name line and can catch that content on a misaligned
// band, which the charset then has nowhere honest to put and reclassifies as
// digits instead. levelSpec and lastActiveSpec's crops do not have another
// field's content available to bleed in the same way (see their own
// comments in roster.go for the field-specific reasoning); that is the part
// of "safe" this file's superset check cannot see on its own, which is why
// TestVSPointsSpecHasNoCharset below exists to lock in the correction rather
// than assert the same style of property vsPointsSpec used to pass.

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
// so the whitelist has no correct-read character available to strip.
//
// The real value-level basis (fix-round correction: an earlier draft of this
// comment cited "0/53 disagreements between whitelisted and unconstrained
// parses" as if that were reassuring; it is not, because 0/53 unconstrained
// level reads parse at all, so there was no row on which the two conditions
// *could* disagree, and the whitelist does 100% of this field's parsing).
// The actual basis, from roster.go's levelSpec comment: in every one of the
// 6/53 rows the whitelist did rescue, the raw unconstrained text already
// carried the correct digits ("Lyi34"->"Lv34", "Evi35"->"Lv35") -- the
// whitelist only ever repaired the "L"/"v" glyphs, never the digits that
// determine the parsed value. A cross-field probe (this charset run over the
// 53 power and last-active crops instead) fabricated no "Lv##" out of either
// field's content, which is the direct check against the C1 failure mode
// (see this file's header comment) rather than an argument for why it
// shouldn't happen.
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

// TestVSPointsSpecHasNoCharset locks in the fix-round correction (finding
// C1): vsPointsSpec used to carry "0123456789," on the strength of the same
// superset argument that keeps levelSpec and lastActiveSpec below, and that
// argument was insufficient here -- see this file's header comment and
// vsPointsSpec's own doc comment in vs.go for why. Measured directly: 6 of
// 11 real bands cut from a committed VS frame produced a parseable number
// out of text that was not a number, once the whitelist was applied; all 6
// of those failed safely without it. Mirrors TestPowerSpecHasNoCharset.
func TestVSPointsSpecHasNoCharset(t *testing.T) {
	if vsPointsSpec.Charset != "" {
		t.Errorf("vsPointsSpec.Charset = %q, want empty -- see vsPointsSpec's doc comment in vs.go "+
			"before restoring one: a charset here was measured manufacturing a parseable number "+
			"out of non-number text on 6 of 11 real VS rows (task 23 fix-round finding C1)",
			vsPointsSpec.Charset)
	}
}

// TestParsePointsRejectsRealMisreads uses the exact raw OCR strings the
// fix-round review measured from real, misaligned VS points crops (finding
// C1) -- unconstrained text task 23's own re-audit reproduced independently
// against a different band of the same frame (see the task report). None of
// these are a points read at all (two rows' worth of name/points text
// bleeding together), so all six must route to review.
func TestParsePointsRejectsRealMisreads(t *testing.T) {
	for _, in := range []string{
		"24959 n459 3473", "2449 3\" 743 £70", "44357 OGLE WEF.",
		"ia 30k JIS", "ni7 Fost nai", "Ox 377 1049",
	} {
		if _, err := ParsePoints(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePoints(%q) did not route to review, want ErrUnparseable", in)
		}
	}
}

// TestParsePointsRejectsTheWhitelistLaunderedShapes is points' version of
// TestParsePowerRejectsDecimalLessValues: the exact strings vsPointsSpec's
// former charset whitelist produced from the misread crops above --
// "24959 n459 3473" became "249594593473", and so on. pointsRe's comma-
// grouping requirement (see its own doc comment in parse.go) is
// defense-in-depth against these reappearing by some other route, same as
// powerRe's mandatory decimal is for power, and with the same kind of gap:
// it catches every multi-group fabrication here, but a garbage read that
// happens to collapse to three or fewer digits -- "ni7 Fost nai" laundered
// to a bare "7" -- still satisfies the single-group case structurally, the
// same shape of survivor "36.0M" is for powerRe (see ParsePower's doc
// comment). That case is intentionally not asserted here as a rejection;
// asserting it would misstate what this check buys.
func TestParsePointsRejectsTheWhitelistLaunderedShapes(t *testing.T) {
	for _, in := range []string{
		"249594593473", "2449374370", "44357", "3074", "73771049",
	} {
		if _, err := ParsePoints(in); !errors.Is(err, ErrUnparseable) {
			t.Errorf("ParsePoints(%q) did not reject an ungrouped multi-digit run, want ErrUnparseable", in)
		}
	}
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

// TestGroupHeaderSpecHasNoCharset is the third field in this file to have a
// whitelist ruled out by measurement rather than by argument, and the first
// where the measurement was taken BEFORE anyone proposed one.
//
// A digit whitelist is the obvious thing to reach for on this field: the group
// header's "N/M" count is digits and a slash, and 21 of capture 1's 61 frames
// fail to read theirs at all ("1/11" coming back as "VN", "VL", "Wu", "U/L" --
// see groupHeaderRegion's doc comment in roster.go). So it was measured, over
// every frame of that capture, through both candidate rectangles, and it is a
// committed mode rather than a note: `make probe-roster
// PROBE_ARGS=-roster.headeropts`.
//
// It does not read R2's count either. What it does instead is FABRICATE:
// through groupHeaderRegion at gray+thr x2, "0123456789/" turns four of those
// 21 unreadable frames into a count-shaped token -- "2 1/1" on seq 29, 44 and
// 45, "82 1/1" on seq 60 -- and parseGroupHeader accepts every one of them as
// a total of 1 for a group of 11. That is the direction of error this package has no
// defence against downstream -- total feeds groupTracker.expected, and
// gt.matchedOrCreated < gt.expected gates member creation, so an
// under-count silently stops the rest of the group being created (task 24's
// review measured a fabricated 6 against a real 64-member group costing 58
// members). N <= M cannot catch it, because 1/1 is perfectly coherent.
//
// The unconstrained read is not merely as good, it is strictly safer: without
// the whitelist those four frames produce no count-shaped token at all and
// route to review, which is recoverable. This test exists because a comment
// recording that measurement survives exactly until someone adds the field
// without reading it.
func TestGroupHeaderSpecHasNoCharset(t *testing.T) {
	if groupHeaderSpec.Charset != "" {
		t.Errorf("groupHeaderSpec.Charset = %q, want empty -- a whitelist here was measured "+
			"FABRICATING a count, not fixing one: \"0123456789/\" turned four unreadable R2 headers "+
			"into a count-shaped token (\"2 1/1\" on three frames, \"82 1/1\" on the fourth), each "+
			"parsing to a total of 1 for a group of 11, which parseGroupHeader accepts "+
			"because 1/1 is coherent. total feeds groupTracker.expected and gates member creation, "+
			"so that under-count silently stops the other 10 members being created. Re-run "+
			"`make probe-roster PROBE_ARGS=-roster.headeropts` before restoring one, and read "+
			"groupHeaderRegion's doc comment in roster.go", groupHeaderSpec.Charset)
	}
}
