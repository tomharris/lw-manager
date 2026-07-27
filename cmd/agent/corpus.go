package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
)

func corpusUsage() {
	fmt.Fprint(os.Stderr, `usage: agent corpus <index|push|pull> [flags]

  index  regenerate index.yaml from the directory tree
  push   upload frames the blob store does not already hold
  pull   materialize frames named in index.yaml
`)
}

func runCorpus(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		corpusUsage()
		return fmt.Errorf("a subcommand is required")
	}

	fs := flag.NewFlagSet("corpus", flag.ExitOnError)
	root := fs.String("corpus", "fixtures/corpus", "corpus root directory")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	store := corpus.New(*root)

	switch args[0] {
	case "index":
		// A human editing the tree over SSH can `cp` a screenshot straight
		// into a label directory instead of going through Add — the package
		// doc explicitly invites this. Store.List already skips a file whose
		// name is not a content hash, so it would never silently corrupt
		// index.yaml, but silently dropping it here is still the wrong
		// response from the one tool whose job is producing a trustworthy
		// index: name it and refuse, the same way a cross-label duplicate
		// does below.
		if strays, err := store.StrayFiles(); err != nil {
			return err
		} else if len(strays) > 0 {
			return fmt.Errorf("corpus: %d file(s) not named by content hash, refusing to write %s: %v (rename each to its sha256 or remove it)",
				len(strays), store.IndexPath(), strays)
		}

		prev, err := store.ReadIndex()
		if err != nil {
			return err
		}
		frames, err := store.All()
		if err != nil {
			return err
		}
		// A human editing the tree over SSH can `cp` instead of `mv`, leaving
		// the same bytes filed under two labels. Reindex does not pick a
		// winner, and neither do we: fail loudly and name the hashes rather
		// than silently double-recording a frame under two scores, one of
		// which is necessarily wrong.
		if dupes := corpus.Duplicates(frames); len(dupes) > 0 {
			return fmt.Errorf("corpus: %d frame(s) filed under more than one label, refusing to write %s: %v (delete the copy under the wrong label)",
				len(dupes), store.IndexPath(), dupes)
		}
		idx := corpus.Reindex(prev, frames, nil)
		if err := store.WriteIndex(idx); err != nil {
			return err
		}
		fmt.Printf("indexed=%d path=%s\n", len(idx.Frames), store.IndexPath())
		return nil

	case "push":
		blobs, err := blob.New(ctx, cfg.Blob)
		if err != nil {
			return err
		}
		n, err := corpus.Push(ctx, store, blobs)
		if err != nil {
			return err
		}
		fmt.Printf("uploaded=%d\n", n)
		return nil

	case "pull":
		blobs, err := blob.New(ctx, cfg.Blob)
		if err != nil {
			return err
		}
		idx, err := store.ReadIndex()
		if err != nil {
			return err
		}
		n, err := corpus.Pull(ctx, store, blobs, idx)
		if err != nil {
			return err
		}
		fmt.Printf("fetched=%d indexed=%d\n", n, len(idx.Frames))
		return nil

	default:
		corpusUsage()
		return fmt.Errorf("unknown corpus subcommand %q", args[0])
	}
}
