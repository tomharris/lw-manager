package roster

import "testing"

func TestTokenSetRatioIsPerfectOnEqualStrings(t *testing.T) {
	if got := TokenSetRatio("kain445", "kain445"); got != 100 {
		t.Errorf("got %d, want 100", got)
	}
}

func TestTokenSetRatioIgnoresTokenOrder(t *testing.T) {
	if got := TokenSetRatio("beast the mq", "mq the beast"); got != 100 {
		t.Errorf("got %d, want 100 — token order must not matter", got)
	}
}

func TestTokenSetRatioToleratesOneBadCharacter(t *testing.T) {
	// The realistic OCR failure: one glyph misread. This must stay well above
	// the review floor or every capture floods the queue.
	if got := TokenSetRatio("kaln445", "kain445"); got < ReviewFloor {
		t.Errorf("got %d, want >= %d", got, ReviewFloor)
	}
}

func TestTokenSetRatioSeparatesDifferentNames(t *testing.T) {
	if got := TokenSetRatio("kalor13", "kain445"); got >= AutoAccept {
		t.Errorf("got %d, want < %d — distinct members must not auto-accept", got, AutoAccept)
	}
}

func TestTokenSetRatioCollapsesLetterSpacing(t *testing.T) {
	// This is the path TestNormalizeCollapsesLetterSpacing does not cover:
	// the actual comparison Rank uses, not Normalize in isolation. A prior
	// version of TokenSetRatio joined sorted tokens with no separator, which
	// alphabetizes the individual letters of a letter-spaced read into
	// "cehillm" instead of collapsing it to "michell" — silently dropping a
	// real member instead of matching or even queueing them for review.
	if got := TokenSetRatio("M I C H E L L", "MICHELL"); got != 100 {
		t.Errorf("got %d, want 100", got)
	}
}

func TestTokenSetRatioEmptyNormalizationNeverMatches(t *testing.T) {
	if got := TokenSetRatio("", "x"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	if got := TokenSetRatio("", ""); got != 0 {
		t.Errorf("got %d, want 0 — an empty normalization is not evidence of a match", got)
	}
}

func TestRankPrefersAnAliasOverAWeakerNameMatch(t *testing.T) {
	members := []Member{
		{ID: 1, Name: "Zero Orca", Aliases: []string{"zerooroa"}},
		{ID: 2, Name: "Zebra"},
	}
	got := Rank("zerooroa", members)
	if len(got) == 0 || got[0].MemberID != 1 {
		t.Fatalf("want member 1 first, got %+v", got)
	}
	if got[0].Score != 100 {
		t.Errorf("alias should match exactly, got %d", got[0].Score)
	}
}

func TestRankIsSortedDescending(t *testing.T) {
	members := []Member{
		{ID: 1, Name: "Kalor13"},
		{ID: 2, Name: "Kain445"},
		{ID: 3, Name: "Kain446"},
	}
	got := Rank("kain445", members)
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Fatalf("not sorted descending: %+v", got)
		}
	}
	if got[0].MemberID != 2 {
		t.Errorf("want member 2 first, got %d", got[0].MemberID)
	}
}

func TestRankOnAnEmptyRosterReturnsNoCandidates(t *testing.T) {
	if got := Rank("anyone", nil); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

func TestRankMatchesALetterSpacedName(t *testing.T) {
	// The regression test for the dropped-row bug: assert through Rank, the
	// path that actually runs, not through Normalize in isolation.
	members := []Member{
		{ID: 1, Name: "MICHELL"},
		{ID: 2, Name: "Someone Else"},
	}
	got := Rank("M I C H E L L", members)
	if len(got) == 0 || got[0].MemberID != 1 {
		t.Fatalf("want member 1 first, got %+v", got)
	}
	if got[0].Score < AutoAccept {
		t.Errorf("got score %d, want >= %d", got[0].Score, AutoAccept)
	}
}

// The case this was built for: rank 31 of capture 6. The stored name carries
// Greek koppa decoration; OCR reads the koppas as ">>", which Normalize drops
// as punctuation while keeping the stored koppas as letters. The two then
// share almost nothing.
func TestTokenSetRatioMatchesADecoratedNameAgainstAnUndecoratedRead(t *testing.T) {
	got := TokenSetRatio("ϟϟ Leo ϟϟ", ">> Lea >>")
	if got < ResidualFloor {
		t.Errorf("TokenSetRatio = %d, want >= ResidualFloor (%d); without decoration stripping this scores 28", got, ResidualFloor)
	}
	if got >= AutoAccept {
		t.Errorf("TokenSetRatio = %d, but 'Leo' vs 'Lea' is a real one-character disagreement and must not auto-accept", got)
	}
}

// Purely additive, and that is the safety property worth asserting rather
// than assuming: the decoration comparison is taken as a MAXIMUM alongside
// the other two, so it can raise a score and can never lower one. A pair that
// matched before still matches at least as well.
func TestDecorationStrippingNeverLowersAScore(t *testing.T) {
	pairs := [][2]string{
		{"GersonGamer", "GersonGamer"},
		{"MICHELL", "M I C H E L L"},
		{"ALBAN80", "ALBANSO"},
		{"한씨아저씨", "한씨아저씨"},
		{"mq the beast", "MQ the Beast"},
		{"ΔKΔŽΔ", "AKAZA"},
	}
	for _, p := range pairs {
		if got := TokenSetRatio(p[0], p[1]); got < ratio(Normalize(p[0]), Normalize(p[1])) {
			t.Errorf("TokenSetRatio(%q, %q) = %d, below the undecorated comparison", p[0], p[1], got)
		}
	}
}

// A member whose name is only decoration must not become matchable against
// everything by collapsing to the empty string, which is what a naive strip
// would do. Empty normalizations score 0 by construction; this asserts the
// strip does not manufacture one.
func TestTokenSetRatioDoesNotCollapseAScriptOnlyNameToEmpty(t *testing.T) {
	if got := TokenSetRatio("한씨아저씨", "한씨아저씨"); got != 100 {
		t.Errorf("a script-only name no longer matches itself: %d", got)
	}
	if got := TokenSetRatio("한씨아저씨", "Innovo"); got != 0 {
		t.Errorf("a script-only name matched an unrelated Latin name at %d", got)
	}
}

// Pins the ORDERING inside TokenSetRatio, which the direct stripDecoration
// tests cannot: they call it on Normalize's output by construction, so they
// are true whatever the scoring path does.
//
// The ordering needs its own guard precisely because the decoration score is
// taken as a maximum. A strip applied to raw text instead of normalized text
// does not break a match loudly -- it reduces "ϟϟ ΔKΔŽΔ ϟϟ" to "K", scores
// near zero, loses the maximum, and contributes NOTHING while every existing
// test still passes. Silent uselessness is the failure mode here, not a
// regression, and only a pair whose sole route to a match runs through both
// mechanisms at once can detect it.
//
// The name is synthetic -- capture 6 has decorated names and homoglyph names
// but none that is both. It is a regression test for an ordering constraint,
// not a claim about the roster.
func TestDecorationStrippingSeesHomoglyphsAlreadyFolded(t *testing.T) {
	if got := TokenSetRatio("ϟϟ ΔKΔŽΔ ϟϟ", "AKAZA"); got != 100 {
		t.Errorf("TokenSetRatio = %d, want 100; the koppas must strip and the deltas must fold, which only happens if stripping runs on normalized text", got)
	}
}
