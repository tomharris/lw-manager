package corpus

import (
	"context"
	"errors"
	"fmt"

	"github.com/tomharris/lw-manager/internal/blob"
)

// Push uploads every frame in the tree that the blob store does not already
// hold. Content addressing makes it idempotent, so it is safe to run after
// every capture session.
func Push(ctx context.Context, s *Store, bs blob.Store) (int, error) {
	frames, err := s.All()
	if err != nil {
		return 0, err
	}
	uploaded := 0
	for _, f := range frames {
		exists, err := bs.Exists(ctx, blob.Key(f.Hash))
		if err != nil {
			return uploaded, fmt.Errorf("corpus: checking blob for %s: %w", f.Hash, err)
		}
		if exists {
			continue
		}
		data, err := s.Read(f.Hash)
		if err != nil {
			return uploaded, err
		}
		if _, _, err := blob.PutContent(ctx, bs, data); err != nil {
			return uploaded, fmt.Errorf("corpus: uploading %s: %w", f.Hash, err)
		}
		uploaded++
	}
	return uploaded, nil
}

// Pull materializes every frame named in idx that is missing from the tree.
//
// A frame in the index with no blob behind it means someone committed an
// index without pushing. That fails loudly: silently producing a short corpus
// would surface later as a gate failure with no obvious cause.
func Pull(ctx context.Context, s *Store, bs blob.Store, idx Index) (int, error) {
	fetched := 0
	for _, e := range idx.Frames {
		switch _, err := s.Find(e.Hash); {
		case err == nil:
			continue
		case !errors.Is(err, ErrNotFound):
			return fetched, err
		}
		data, err := blob.GetContent(ctx, bs, blob.Key(e.Hash), e.Hash)
		if err != nil {
			return fetched, fmt.Errorf("corpus: fetching %s (label %q): %w", e.Hash, e.Label, err)
		}
		if _, _, err := s.Add(e.Label, data); err != nil {
			return fetched, err
		}
		fetched++
	}
	return fetched, nil
}
