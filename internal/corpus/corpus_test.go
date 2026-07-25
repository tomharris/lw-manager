package corpus_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/corpus"
)

func TestAddNamesFrameByFullContentHash(t *testing.T) {
	s := corpus.New(t.TempDir())
	data := []byte("frame-bytes")

	f, added, err := s.Add(corpus.Unsorted, data)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !added {
		t.Fatal("first Add reported the frame as a duplicate")
	}
	if f.Hash != blob.Sum(data) {
		t.Fatalf("Hash = %q, want %q", f.Hash, blob.Sum(data))
	}
	if got, want := filepath.Base(f.Path), blob.Sum(data)+".png"; got != want {
		t.Fatalf("filename = %q, want %q", got, want)
	}
}

// A burst capture of an idle screen produces the same bytes over and over.
// Those are noise, not observations: keeping them would weight the accuracy
// denominator toward whatever screen the phone sat on.
func TestAddDeduplicatesAcrossLabels(t *testing.T) {
	s := corpus.New(t.TempDir())
	data := []byte("frame-bytes")

	first, _, err := s.Add("base", data)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	again, added, err := s.Add(corpus.Unsorted, data)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if added {
		t.Fatal("re-adding identical bytes reported a new frame")
	}
	if again.Label != "base" || again.Path != first.Path {
		t.Fatalf("duplicate resolved to %+v, want the existing %+v", again, first)
	}
}

// Labels arrive from the studio's browser UI, so this is a boundary check.
func TestAddRejectsLabelsThatEscapeTheRoot(t *testing.T) {
	s := corpus.New(t.TempDir())
	for _, bad := range []string{"../etc", "a/b", "", ".", "Base", "has space"} {
		if _, _, err := s.Add(bad, []byte("x")); !errors.Is(err, corpus.ErrInvalidLabel) {
			t.Errorf("Add(%q) error = %v, want ErrInvalidLabel", bad, err)
		}
	}
}

func TestRelabelMovesTheFrame(t *testing.T) {
	s := corpus.New(t.TempDir())
	f, _, err := s.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	moved, err := s.Relabel(f.Hash, "alliance")
	if err != nil {
		t.Fatalf("Relabel: %v", err)
	}
	if moved.Label != "alliance" {
		t.Fatalf("Label = %q, want alliance", moved.Label)
	}

	unsorted, err := s.List(corpus.Unsorted)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(unsorted) != 0 {
		t.Fatalf("frame still in %s: %+v", corpus.Unsorted, unsorted)
	}
	found, err := s.Find(f.Hash)
	if err != nil {
		t.Fatalf("Find after relabel: %v", err)
	}
	if found.Label != "alliance" {
		t.Fatalf("Find reports label %q, want alliance", found.Label)
	}
}

func TestFindReportsErrNotFoundForUnknownHash(t *testing.T) {
	s := corpus.New(t.TempDir())
	if _, err := s.Find("deadbeef"); !errors.Is(err, corpus.ErrNotFound) {
		t.Fatalf("Find error = %v, want ErrNotFound", err)
	}
}

func TestListAndAllAreSortedAndStable(t *testing.T) {
	s := corpus.New(t.TempDir())
	for _, b := range [][]byte{[]byte("c"), []byte("a"), []byte("b")} {
		if _, _, err := s.Add("base", b); err != nil {
			t.Fatalf("Add(%q): %v", b, err)
		}
	}
	if _, _, err := s.Add(corpus.None, []byte("z")); err != nil {
		t.Fatalf("Add negative: %v", err)
	}

	got, err := s.List("base")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d frames, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Hash >= got[i].Hash {
			t.Fatalf("List not sorted by hash: %q then %q", got[i-1].Hash, got[i].Hash)
		}
	}

	all, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("All returned %d frames, want 4", len(all))
	}
	if all[0].Label != corpus.None {
		t.Fatalf("All not sorted by label first: got %q", all[0].Label)
	}
}

func TestListAndLabelsOnAnEmptyRoot(t *testing.T) {
	s := corpus.New(filepath.Join(t.TempDir(), "does-not-exist"))

	labels, err := s.Labels()
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(labels) != 0 {
		t.Fatalf("Labels = %v, want none", labels)
	}
	frames, err := s.List("base")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("List = %v, want none", frames)
	}
}

func TestReadReturnsTheOriginalBytes(t *testing.T) {
	s := corpus.New(t.TempDir())
	data := []byte("frame-bytes")
	f, _, err := s.Add("base", data)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := s.Read(f.Hash)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Read = %q, want %q", got, data)
	}
}
