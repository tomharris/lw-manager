// Package corpus stores labeled screenshots for device-free vision tests.
//
// The label is the directory: <root>/<label>/<sha256>.png. There is no
// sidecar metadata to fall out of sync with the tree, so a mislabel is fixed
// with mv over SSH and the corpus stays inspectable without this package.
//
// Frames are named by the full SHA-256 of their bytes. That deduplicates a
// burst capture of an idle screen down to one frame, and it makes a filename
// directly usable as a blob.Key input.
//
// Note the deliberate divergence from the screenshots table, where identical
// bytes still earn their own row because each capture is a distinct
// observation. Here a duplicate frame is not an observation, it is noise that
// would skew the accuracy denominator toward whichever screen the phone
// happened to idle on.
package corpus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tomharris/lw-manager/internal/blob"
)

const (
	// Unsorted holds freshly captured frames that carry no label yet.
	Unsorted = "_unsorted"
	// None labels negatives: frames the recognizer must refuse to identify.
	// They are part of the gate, not a separate figure — without them the
	// gate is passable with thresholds so loose every frame matches
	// something, and acting on a misidentified screen is the blind tap
	// invariant #3 forbids.
	None = "_none"
)

var (
	// ErrNotFound reports that no frame in the corpus carries a hash.
	ErrNotFound = errors.New("corpus: frame not found")
	// ErrInvalidLabel reports a label that is not a safe directory name.
	ErrInvalidLabel = errors.New("corpus: invalid label")
	// ErrInvalidHash reports a hash that is not a well-formed content digest.
	ErrInvalidHash = errors.New("corpus: invalid hash")
)

// validLabel constrains labels to a shape that cannot escape the corpus root.
// Labels arrive from the studio's browser UI, so this is a boundary check and
// not a style rule: "../../etc" must never become a directory name.
var validLabel = regexp.MustCompile(`^_?[a-z0-9][a-z0-9_]*$`)

// validHash matches exactly what blob.Sum produces: a full lowercase-hex
// SHA-256 digest. Nothing else is a legitimate hash this package should ever
// look up.
var validHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CheckHash validates hash, returning ErrInvalidHash if it is not a
// well-formed content digest.
//
// A hash reaches this package from a URL path segment (the studio's
// /frame/{hash} route), so this is a boundary check, not a style rule:
// filepath.Join(s.root, label, hash+".png") must never see a hash containing
// "/" or "..", or a caller-controlled string walks the join straight out of
// the corpus root.
func CheckHash(hash string) error {
	if !validHash.MatchString(hash) {
		return fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	return nil
}

// Frame is one corpus image on disk.
type Frame struct {
	Hash  string // full hex SHA-256 of the file's bytes
	Label string // the directory it sits in
	Path  string // where it lives on disk
}

// Store is a corpus rooted at a directory.
type Store struct{ root string }

// New returns a store over root. The directory need not exist yet.
func New(root string) *Store { return &Store{root: root} }

// Root reports the corpus root directory.
func (s *Store) Root() string { return s.root }

// CheckLabel validates a label, returning ErrInvalidLabel if it is not a safe
// directory name.
func CheckLabel(label string) error {
	if !validLabel.MatchString(label) {
		return fmt.Errorf("%w: %q", ErrInvalidLabel, label)
	}
	return nil
}

// Add writes data under label, named by its content hash. It reports
// added=false and the existing frame when the same bytes are already
// anywhere in the corpus, whatever label they carry.
func (s *Store) Add(label string, data []byte) (Frame, bool, error) {
	if err := CheckLabel(label); err != nil {
		return Frame{}, false, err
	}
	sum := blob.Sum(data)

	switch existing, err := s.Find(sum); {
	case err == nil:
		return existing, false, nil
	case !errors.Is(err, ErrNotFound):
		return Frame{}, false, err
	}

	dir := filepath.Join(s.root, label)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Frame{}, false, fmt.Errorf("corpus: creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, sum+".png")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return Frame{}, false, fmt.Errorf("corpus: writing %s: %w", path, err)
	}
	return Frame{Hash: sum, Label: label, Path: path}, true, nil
}

// Find locates a frame by its full hash, searching every label.
func (s *Store) Find(hash string) (Frame, error) {
	if err := CheckHash(hash); err != nil {
		return Frame{}, err
	}
	labels, err := s.Labels()
	if err != nil {
		return Frame{}, err
	}
	for _, l := range labels {
		path := filepath.Join(s.root, l, hash+".png")
		switch _, err := os.Stat(path); {
		case err == nil:
			return Frame{Hash: hash, Label: l, Path: path}, nil
		case !errors.Is(err, os.ErrNotExist):
			return Frame{}, fmt.Errorf("corpus: stat %s: %w", path, err)
		}
	}
	return Frame{}, fmt.Errorf("%w: %s", ErrNotFound, hash)
}

// Labels returns every label directory present, sorted. A corpus root that
// does not exist yet has no labels rather than being an error: `agent record`
// creates it on first capture.
func (s *Store) Labels() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("corpus: reading %s: %w", s.root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && validLabel.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// List returns every frame under label, sorted by hash.
func (s *Store) List(label string) ([]Frame, error) {
	if err := CheckLabel(label); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, label)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("corpus: reading %s: %w", dir, err)
	}
	var out []Frame
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".png") {
			continue
		}
		out = append(out, Frame{
			Hash:  strings.TrimSuffix(name, ".png"),
			Label: label,
			Path:  filepath.Join(dir, name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out, nil
}

// All returns every frame in the corpus, sorted by label then hash. The
// ordering is deterministic so a scoring run over the corpus is reproducible.
func (s *Store) All() ([]Frame, error) {
	labels, err := s.Labels()
	if err != nil {
		return nil, err
	}
	var out []Frame
	for _, l := range labels {
		frames, err := s.List(l)
		if err != nil {
			return nil, err
		}
		out = append(out, frames...)
	}
	return out, nil
}

// Relabel moves a frame into a different label directory.
func (s *Store) Relabel(hash, label string) (Frame, error) {
	if err := CheckHash(hash); err != nil {
		return Frame{}, err
	}
	if err := CheckLabel(label); err != nil {
		return Frame{}, err
	}
	f, err := s.Find(hash)
	if err != nil {
		return Frame{}, err
	}
	if f.Label == label {
		return f, nil
	}
	dir := filepath.Join(s.root, label)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Frame{}, fmt.Errorf("corpus: creating %s: %w", dir, err)
	}
	dst := filepath.Join(dir, hash+".png")
	if err := os.Rename(f.Path, dst); err != nil {
		return Frame{}, fmt.Errorf("corpus: moving %s to %s: %w", f.Path, dst, err)
	}
	return Frame{Hash: hash, Label: label, Path: dst}, nil
}

// Read returns a frame's bytes.
func (s *Store) Read(hash string) ([]byte, error) {
	if err := CheckHash(hash); err != nil {
		return nil, err
	}
	f, err := s.Find(hash)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, fmt.Errorf("corpus: reading %s: %w", f.Path, err)
	}
	return data, nil
}
