package vision

import (
	"bytes"
	"fmt"
	"image/png"

	"github.com/tomharris/lw-manager/internal/corpus"
)

// LoadCorpusFrames decodes every labeled frame in store into a Frame ready
// for Evaluate. Unsorted frames are skipped: they carry no ground truth, so
// scoring them would be meaningless.
//
// Both `agent score` and the M1 gate test (corpus_test.go, behind the
// "corpus" build tag) need exactly this loading step. Keeping one
// implementation here — rather than each duplicating the loop — means a
// change to what "the corpus" means for scoring purposes cannot drift
// between the CLI an operator runs by hand and the gate that decides whether
// the milestone is actually done.
func LoadCorpusFrames(store *corpus.Store) ([]Frame, error) {
	all, err := store.All()
	if err != nil {
		return nil, err
	}
	out := make([]Frame, 0, len(all))
	for _, f := range all {
		if f.Label == corpus.Unsorted {
			continue
		}
		data, err := store.Read(f.Hash)
		if err != nil {
			return nil, err
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("decoding corpus frame %s (label %q): %w", f.Hash, f.Label, err)
		}
		out = append(out, Frame{Hash: f.Hash, Label: f.Label, Image: img})
	}
	return out, nil
}
