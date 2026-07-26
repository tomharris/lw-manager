package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
)

// A stray file that reached the tree by `cp` instead of `Store.Add` (the
// package doc explicitly invites hand-editing over SSH) must not reach
// index.yaml — Store.List already skips it — but `agent corpus index` also
// must not just quietly produce a shorter index than expected: it has to
// name the file and refuse, the same way it already does for a cross-label
// duplicate.
func TestRunCorpusIndexRefusesOnAStrayNonHashFile(t *testing.T) {
	root := t.TempDir()
	store := corpus.New(root)
	if _, _, err := store.Add("base", []byte("real-frame")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "base", "screenshot.png"), []byte("stray"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := runCorpus(context.Background(), config.Config{}, []string{"index", "--corpus", root})
	if err == nil {
		t.Fatal("runCorpus(index) succeeded despite a stray non-hash file in the tree")
	}
	if !strings.Contains(err.Error(), "screenshot.png") {
		t.Fatalf("error %q does not name the stray file", err)
	}
	if _, err := os.Stat(store.IndexPath()); !os.IsNotExist(err) {
		t.Fatal("index.yaml was written despite the stray file")
	}
}

func TestRunCorpusIndexSucceedsOnAnOrdinaryCorpus(t *testing.T) {
	root := t.TempDir()
	store := corpus.New(root)
	if _, _, err := store.Add("base", []byte("a")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := runCorpus(context.Background(), config.Config{}, []string{"index", "--corpus", root}); err != nil {
		t.Fatalf("runCorpus(index): %v", err)
	}
	idx, err := store.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(idx.Frames) != 1 {
		t.Fatalf("index has %d frames, want 1", len(idx.Frames))
	}
}
