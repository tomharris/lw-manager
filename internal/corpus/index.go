package corpus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// indexFile is the name of the committed projection of the corpus tree.
const indexFile = "index.yaml"

// Meta is capture-time metadata that cannot be recovered from a PNG's bytes.
type Meta struct {
	Width       int
	Height      int
	CapturedAt  time.Time
	Device      string
	GameVersion string
}

// Entry is one frame's committed record.
//
// This metadata is in the index precisely because the PNGs are not in git.
// Without GameVersion, a future reader cannot tell whether a corpus predates
// a game update — the single most likely cause of a gate that used to pass
// and now does not.
type Entry struct {
	Hash        string    `yaml:"hash"`
	Label       string    `yaml:"label"`
	Width       int       `yaml:"width"`
	Height      int       `yaml:"height"`
	CapturedAt  time.Time `yaml:"captured_at"`
	Device      string    `yaml:"device"`
	GameVersion string    `yaml:"game_version"`
}

// Index is the committed projection of the corpus tree.
type Index struct {
	Frames []Entry `yaml:"frames"`
}

// Reindex rebuilds an index from the frames actually on disk.
//
// Labels always come from the tree, because the tree is what a human edits
// with mv. Capture metadata comes from metas when the caller just captured
// the frame, otherwise from prev, otherwise blank. Entries whose file is gone
// are dropped: someone deleted that frame deliberately, and keeping the entry
// would have `corpus pull` fetch it straight back.
func Reindex(prev Index, frames []Frame, metas map[string]Meta) Index {
	previous := make(map[string]Entry, len(prev.Frames))
	for _, e := range prev.Frames {
		previous[e.Hash] = e
	}

	out := Index{Frames: make([]Entry, 0, len(frames))}
	for _, f := range frames {
		e := previous[f.Hash] // zero value when unknown, which is the blank case
		e.Hash = f.Hash
		e.Label = f.Label
		if m, ok := metas[f.Hash]; ok {
			e.Width, e.Height = m.Width, m.Height
			e.CapturedAt = m.CapturedAt
			e.Device, e.GameVersion = m.Device, m.GameVersion
		}
		out.Frames = append(out.Frames, e)
	}
	sort.Slice(out.Frames, func(i, j int) bool { return out.Frames[i].Hash < out.Frames[j].Hash })
	return out
}

// IndexPath reports where the index lives.
func (s *Store) IndexPath() string { return filepath.Join(s.root, indexFile) }

// ReadIndex loads the committed index. A missing file is an empty index, not
// an error: a fresh clone has not pulled a corpus yet.
func (s *Store) ReadIndex() (Index, error) {
	raw, err := os.ReadFile(s.IndexPath())
	if errors.Is(err, os.ErrNotExist) {
		return Index{}, nil
	}
	if err != nil {
		return Index{}, fmt.Errorf("corpus: reading %s: %w", s.IndexPath(), err)
	}
	var idx Index
	if err := yaml.Unmarshal(raw, &idx); err != nil {
		return Index{}, fmt.Errorf("corpus: parsing %s: %w", s.IndexPath(), err)
	}
	return idx, nil
}

// WriteIndex persists the index.
func (s *Store) WriteIndex(idx Index) error {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("corpus: creating %s: %w", s.root, err)
	}
	raw, err := yaml.Marshal(idx)
	if err != nil {
		return fmt.Errorf("corpus: encoding index: %w", err)
	}
	if err := os.WriteFile(s.IndexPath(), raw, 0o644); err != nil {
		return fmt.Errorf("corpus: writing %s: %w", s.IndexPath(), err)
	}
	return nil
}
