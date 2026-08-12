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
