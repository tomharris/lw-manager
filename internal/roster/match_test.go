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
