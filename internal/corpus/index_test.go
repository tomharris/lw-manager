package corpus_test

import (
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/corpus"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return ts
}

// The tree is what a human edits with mv, so it wins on labels.
func TestReindexTakesLabelsFromTheTree(t *testing.T) {
	prev := corpus.Index{Frames: []corpus.Entry{
		{Hash: "aa", Label: "_unsorted", Width: 1080, Height: 2400, Device: "Pixel"},
	}}
	frames := []corpus.Frame{{Hash: "aa", Label: "alliance"}}

	got := corpus.Reindex(prev, frames, nil)

	if len(got.Frames) != 1 {
		t.Fatalf("Frames = %+v, want one entry", got.Frames)
	}
	if got.Frames[0].Label != "alliance" {
		t.Fatalf("Label = %q, want alliance (from the tree)", got.Frames[0].Label)
	}
	if got.Frames[0].Device != "Pixel" {
		t.Fatalf("Device = %q, want Pixel preserved from the previous index", got.Frames[0].Device)
	}
}

func TestReindexPrefersFreshMetaOverThePreviousIndex(t *testing.T) {
	prev := corpus.Index{Frames: []corpus.Entry{
		{Hash: "aa", Label: "base", Device: "old", GameVersion: "1.0"},
	}}
	frames := []corpus.Frame{{Hash: "aa", Label: "base"}}
	metas := map[string]corpus.Meta{
		"aa": {Width: 1080, Height: 2400, Device: "new", GameVersion: "2.0",
			CapturedAt: mustTime(t, "2026-07-25T14:03:11Z")},
	}

	got := corpus.Reindex(prev, frames, metas)

	e := got.Frames[0]
	if e.Device != "new" || e.GameVersion != "2.0" || e.Width != 1080 || e.Height != 2400 {
		t.Fatalf("entry = %+v, want the fresh meta", e)
	}
}

// Reindex replaces the whole metadata portion of an entry when a fresh Meta
// is present, rather than merging field by field. A fresh capture knows the
// complete truth about the frame it just took, so a Meta field left at its
// zero value (e.g. GameVersion not populated by the caller) must blank
// whatever the previous index held there, not fall back to it. Otherwise a
// stale field from a previous capture could survive under a new capture's
// entry with no way to tell it apart from a genuine fresh value.
func TestReindexReplacesTheWholeMetaEntryNotFieldByField(t *testing.T) {
	prev := corpus.Index{Frames: []corpus.Entry{
		{Hash: "aa", Label: "base", Width: 1080, Height: 2400, Device: "old", GameVersion: "1.0"},
	}}
	frames := []corpus.Frame{{Hash: "aa", Label: "base"}}
	metas := map[string]corpus.Meta{
		"aa": {Device: "new"}, // Width, Height, GameVersion, CapturedAt left zero
	}

	got := corpus.Reindex(prev, frames, metas)

	e := got.Frames[0]
	if e.Device != "new" {
		t.Fatalf("Device = %q, want new", e.Device)
	}
	if e.Width != 0 || e.Height != 0 || e.GameVersion != "" || !e.CapturedAt.IsZero() {
		t.Fatalf("entry = %+v, want every meta field zeroed by the fresh (partial) meta, not carried from prev", e)
	}
}

// A frame deleted from the tree must leave the index, or the gate would try
// to pull back a frame someone deliberately threw away.
func TestReindexDropsEntriesWithNoFileOnDisk(t *testing.T) {
	prev := corpus.Index{Frames: []corpus.Entry{
		{Hash: "aa", Label: "base"},
		{Hash: "bb", Label: "base"},
	}}
	frames := []corpus.Frame{{Hash: "aa", Label: "base"}}

	got := corpus.Reindex(prev, frames, nil)

	if len(got.Frames) != 1 || got.Frames[0].Hash != "aa" {
		t.Fatalf("Frames = %+v, want only aa", got.Frames)
	}
}

func TestReindexIsSortedByHashForStableDiffs(t *testing.T) {
	frames := []corpus.Frame{
		{Hash: "cc", Label: "base"},
		{Hash: "aa", Label: "mail"},
		{Hash: "bb", Label: "radar"},
	}

	got := corpus.Reindex(corpus.Index{}, frames, nil)

	for i, want := range []string{"aa", "bb", "cc"} {
		if got.Frames[i].Hash != want {
			t.Fatalf("Frames[%d].Hash = %q, want %q", i, got.Frames[i].Hash, want)
		}
	}
}

func TestIndexRoundTripsThroughYAML(t *testing.T) {
	s := corpus.New(t.TempDir())
	want := corpus.Index{Frames: []corpus.Entry{{
		Hash:        "aa",
		Label:       "alliance",
		Width:       1080,
		Height:      2400,
		CapturedAt:  mustTime(t, "2026-07-25T14:03:11Z"),
		Device:      "Pixel 8a",
		GameVersion: "3.2.1",
	}}}

	if err := s.WriteIndex(want); err != nil {
		t.Fatalf("WriteIndex: %v", err)
	}
	got, err := s.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(got.Frames) != 1 {
		t.Fatalf("Frames = %+v, want one entry", got.Frames)
	}
	g, w := got.Frames[0], want.Frames[0]
	if g.Hash != w.Hash || g.Label != w.Label || g.Width != w.Width ||
		g.Height != w.Height || g.Device != w.Device || g.GameVersion != w.GameVersion {
		t.Fatalf("entry = %+v, want %+v", g, w)
	}
	if !g.CapturedAt.Equal(w.CapturedAt) {
		t.Fatalf("CapturedAt = %v, want %v", g.CapturedAt, w.CapturedAt)
	}
}

// A fresh clone has no index yet. That is empty, not broken.
func TestReadIndexOnAMissingFileIsEmpty(t *testing.T) {
	s := corpus.New(t.TempDir())

	got, err := s.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(got.Frames) != 0 {
		t.Fatalf("Frames = %+v, want none", got.Frames)
	}
}

func TestDuplicatesIsEmptyWhenEveryHashHasOneLabel(t *testing.T) {
	frames := []corpus.Frame{
		{Hash: "aa", Label: "base"},
		{Hash: "bb", Label: "radar"},
	}

	got := corpus.Duplicates(frames)

	if len(got) != 0 {
		t.Fatalf("Duplicates = %+v, want none", got)
	}
}

// A cp instead of a mv leaves the same bytes under two label directories.
// Store.Add cannot produce this — it dedups by hash — but hand-editing the
// tree over SSH can.
func TestDuplicatesReportsAHashUnderTwoLabels(t *testing.T) {
	frames := []corpus.Frame{
		{Hash: "aa", Label: "base"},
		{Hash: "aa", Label: "radar"},
	}

	got := corpus.Duplicates(frames)

	if len(got) != 1 || got[0] != "aa" {
		t.Fatalf("Duplicates = %+v, want [aa]", got)
	}
}

func TestDuplicatesReportsAHashOnceEvenUnderThreeLabels(t *testing.T) {
	frames := []corpus.Frame{
		{Hash: "aa", Label: "base"},
		{Hash: "aa", Label: "radar"},
		{Hash: "aa", Label: "mail"},
	}

	got := corpus.Duplicates(frames)

	if len(got) != 1 || got[0] != "aa" {
		t.Fatalf("Duplicates = %+v, want [aa] exactly once", got)
	}
}

func TestDuplicatesReturnsEachDuplicatedHashSorted(t *testing.T) {
	frames := []corpus.Frame{
		{Hash: "cc", Label: "base"},
		{Hash: "cc", Label: "radar"},
		{Hash: "aa", Label: "base"},
		{Hash: "aa", Label: "mail"},
	}

	got := corpus.Duplicates(frames)

	if len(got) != 2 || got[0] != "aa" || got[1] != "cc" {
		t.Fatalf("Duplicates = %+v, want [aa cc]", got)
	}
}
