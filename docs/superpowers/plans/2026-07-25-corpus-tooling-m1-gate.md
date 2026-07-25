# Corpus Tooling and the M1 Gate — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the tooling to capture, label, crop and score a real screenshot corpus, so the M1 gate — screen recognizer ≥ 98% accuracy offline — can be reached and re-reached after every game update.

**Architecture:** A pure-filesystem `internal/corpus` package where the directory name *is* the label and frames are named by content hash. A small LAN-served `internal/studio` HTTP UI for the two jobs that need human eyes (labeling, cropping), because the build host is headless. Scoring stays a batch CLI over pure functions that take score tuples rather than images, so all the interesting logic tests without a single PNG.

**Tech Stack:** Go 1.x, `CGO_ENABLED=0`, `net/http` + `html/template` (stdlib only), `go.yaml.in/yaml/v3`, existing `internal/blob`, `internal/vision`, `internal/transport`.

**Spec:** `docs/superpowers/specs/2026-07-25-corpus-tooling-m1-gate-design.md`

## Global Constraints

- `CGO_ENABLED=0` always. No new dependency may drag in cgo. `make verify-nocgo` must pass.
- No new third-party dependencies. Everything here uses the stdlib plus `go.yaml.in/yaml/v3`, which is already direct.
- No absolute pixel coordinates outside a `Transport` implementation. Crop rectangles are `transport.Rect` with both components in `[0,1]`.
- `context.Context` is the first parameter of anything doing I/O.
- Errors wrap with `%w` and name the device, account, path or key involved.
- All logging goes through `log/slog` to **stderr**. CLI results go to **stdout**.
- Sentinel errors are compared with `errors.Is`/`errors.As`, never by string.
- Sleeps go through the jittered helper. Never bare `time.Sleep`.
- `make test` must pass with no emulator, no adb, no Docker, no tesseract.
- Frames are named `<full-64-char-sha256>.png`. Never a truncated digest.
- Special label constants: `_unsorted` (unlabeled) and `_none` (negatives).

---

## File Structure

**Created:**

| File | Responsibility |
|---|---|
| `internal/corpus/corpus.go` | `Store` over a root dir: add, find, list, relabel, read |
| `internal/corpus/corpus_test.go` | Temp-dir tests for the above |
| `internal/corpus/index.go` | `Index`, `Entry`, `Meta`, pure `Reindex`, read/write `index.yaml` |
| `internal/corpus/index_test.go` | Reindex merge semantics, YAML round-trip |
| `internal/corpus/sync.go` | `Push`/`Pull` between the tree and `blob.Store` |
| `internal/corpus/sync_test.go` | Sync against an in-memory blob store |
| `internal/studio/studio.go` | `Server`, `Options`, `Handler`, token middleware, `RequireToken` |
| `internal/studio/studio_test.go` | `httptest` auth and routing tests |
| `internal/studio/views.go` | `html/template` pages: label grid, labeled browser, crop view |
| `internal/studio/handlers.go` | Frame serving, `POST /label`, `POST /capture`, `POST /crop` |
| `internal/studio/handlers_test.go` | Handler tests with `ReplayTransport` and a fake template writer |
| `internal/vision/manifest_write.go` | `AnchorSpec`, `WriteAnchor`, `SetThresholds` — write + revalidate + rollback |
| `internal/vision/manifest_write_test.go` | Rollback on invalid write, upsert semantics |
| `internal/vision/score.go` | `Prediction`, `Report`, pure `Score`, confusion matrix rendering |
| `internal/vision/score_test.go` | Pure scoring tests, no images |
| `internal/vision/separation.go` | `AnchorObservation`, `Separation`, pure `Separations` |
| `internal/vision/separation_test.go` | Gap, overlap and degenerate-case tests, no images |
| `internal/vision/corpus_test.go` | The gate. `//go:build corpus` |
| `cmd/agent/record.go` | `agent record` |
| `cmd/agent/corpus.go` | `agent corpus index\|push\|pull` |
| `cmd/agent/studio.go` | `agent studio` |
| `cmd/agent/score.go` | `agent score` |

**Modified:**

| File | Change |
|---|---|
| `internal/runtime/jitter.go` | Rename `jitter` → `Jitter` (exported) so non-task code can comply with invariant #7 |
| `internal/runtime/ctx.go:201` | Call site update |
| `internal/runtime/act.go:93` | Call site update |
| `internal/runtime/jitter_test.go` | Call site updates |
| `internal/vision/matcher.go` | Export `Resize` wrapping the existing private `resizeGray` |
| `cmd/agent/main.go` | Register four new subcommands and their usage lines |
| `Makefile` | Add `gate` target |
| `CLAUDE.md` | Document the corpus, the studio, `make gate`, and the dedup divergence |
| `.gitignore` | Ignore `fixtures/corpus/*/` but not `fixtures/corpus/index.yaml` |

---

## Task 1: Corpus store

**Files:**
- Create: `internal/corpus/corpus.go`
- Test: `internal/corpus/corpus_test.go`

**Interfaces:**
- Consumes: `blob.Sum(data []byte) string` from `internal/blob`.
- Produces:
  - `const Unsorted = "_unsorted"`, `const None = "_none"`
  - `var ErrNotFound, ErrInvalidLabel error`
  - `type Frame struct { Hash, Label, Path string }`
  - `func New(root string) *Store`
  - `func (s *Store) Root() string`
  - `func CheckLabel(label string) error`
  - `func (s *Store) Add(label string, data []byte) (Frame, bool, error)`
  - `func (s *Store) Find(hash string) (Frame, error)`
  - `func (s *Store) Labels() ([]string, error)`
  - `func (s *Store) List(label string) ([]Frame, error)`
  - `func (s *Store) All() ([]Frame, error)`
  - `func (s *Store) Relabel(hash, label string) (Frame, error)`
  - `func (s *Store) Read(hash string) ([]byte, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/corpus/corpus_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/corpus/...`
Expected: FAIL — `no Go files in .../internal/corpus` (the package does not exist yet).

- [ ] **Step 3: Write the implementation**

Create `internal/corpus/corpus.go`:

```go
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
)

// validLabel constrains labels to a shape that cannot escape the corpus root.
// Labels arrive from the studio's browser UI, so this is a boundary check and
// not a style rule: "../../etc" must never become a directory name.
var validLabel = regexp.MustCompile(`^_?[a-z0-9][a-z0-9_]*$`)

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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/corpus/... -v`
Expected: PASS, all eight tests.

- [ ] **Step 5: Run the full suite and vet**

Run: `make test && make lint`
Expected: PASS, no vet findings, no gofmt diff.

- [ ] **Step 6: Commit**

```bash
git add internal/corpus/corpus.go internal/corpus/corpus_test.go
git commit -m "Add corpus store: directory-as-label, hash-named frames

The label is the directory and the filename is the full content hash, so
the corpus stays inspectable and fixable with mv over SSH, and identical
frames from a burst capture collapse instead of skewing the accuracy
denominator. Labels are validated as a boundary check because they arrive
from the studio's browser UI."
```

---

## Task 2: Corpus index

**Files:**
- Create: `internal/corpus/index.go`
- Test: `internal/corpus/index_test.go`

**Interfaces:**
- Consumes: `Frame`, `Store.All`, `Store.Root` from Task 1; `go.yaml.in/yaml/v3`.
- Produces:
  - `type Meta struct { Width, Height int; CapturedAt time.Time; Device, GameVersion string }`
  - `type Entry struct { Hash, Label string; Width, Height int; CapturedAt time.Time; Device, GameVersion string }`
  - `type Index struct { Frames []Entry }`
  - `func Reindex(prev Index, frames []Frame, metas map[string]Meta) Index`
  - `func (s *Store) ReadIndex() (Index, error)`
  - `func (s *Store) WriteIndex(idx Index) error`
  - `func (s *Store) IndexPath() string`

**Why `Reindex` is pure and takes three arguments:** labels always come from the
tree, because the tree is what a human edits with `mv`. Capture metadata cannot
be recovered from a PNG's bytes, so it comes from `metas` when the caller just
captured the frame, otherwise from the previous index, otherwise blank. Keeping
that merge in a pure function means the interesting rules test without a
filesystem.

- [ ] **Step 1: Write the failing tests**

Create `internal/corpus/index_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/corpus/... -run 'Index|Reindex'`
Expected: FAIL — `undefined: corpus.Reindex`, `undefined: corpus.Index`.

- [ ] **Step 3: Write the implementation**

Create `internal/corpus/index.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/corpus/... -v`
Expected: PASS, all Task 1 and Task 2 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/corpus/index.go internal/corpus/index_test.go
git commit -m "Add corpus index: committed projection of the tree

Reindex is pure and merges three sources: labels always from the tree
(which a human edits with mv), capture metadata from a fresh capture when
present, otherwise from the previous index. Metadata lives here because the
PNGs are not in git, and game_version is what later explains a gate that
used to pass."
```

---

## Task 3: Corpus blob sync

**Files:**
- Create: `internal/corpus/sync.go`
- Test: `internal/corpus/sync_test.go`

**Interfaces:**
- Consumes: `Store.All`, `Store.Read`, `Store.Add`, `Index`, `Entry` from Tasks 1–2; `blob.Store`, `blob.Key`, `blob.PutContent`, `blob.GetContent` from `internal/blob`.
- Produces:
  - `func Push(ctx context.Context, s *Store, bs blob.Store) (uploaded int, err error)`
  - `func Pull(ctx context.Context, s *Store, bs blob.Store, idx Index) (fetched int, err error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/corpus/sync_test.go`:

```go
package corpus_test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/corpus"
)

// memBlob is an in-memory blob.Store so sync tests need no filesystem
// backend and no MinIO.
type memBlob struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemBlob() *memBlob { return &memBlob{objects: map[string][]byte{}} }

func (m *memBlob) Put(_ context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = data
	return nil
}

func (m *memBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memBlob) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok, nil
}

func TestPushUploadsEveryFrameOnce(t *testing.T) {
	ctx := context.Background()
	s := corpus.New(t.TempDir())
	bs := newMemBlob()
	for _, b := range [][]byte{[]byte("a"), []byte("b")} {
		if _, _, err := s.Add("base", b); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	n, err := corpus.Push(ctx, s, bs)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if n != 2 {
		t.Fatalf("uploaded = %d, want 2", n)
	}

	// Pushing again uploads nothing: content addressing makes it idempotent.
	again, err := corpus.Push(ctx, s, bs)
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if again != 0 {
		t.Fatalf("second push uploaded %d, want 0", again)
	}
}

func TestPullMaterializesMissingFramesIntoTheirLabelDirectories(t *testing.T) {
	ctx := context.Background()
	bs := newMemBlob()
	data := []byte("frame-bytes")
	if _, _, err := blob.PutContent(ctx, bs, data); err != nil {
		t.Fatalf("seeding blob: %v", err)
	}

	s := corpus.New(t.TempDir())
	idx := corpus.Index{Frames: []corpus.Entry{{Hash: blob.Sum(data), Label: "alliance"}}}

	n, err := corpus.Pull(ctx, s, bs, idx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if n != 1 {
		t.Fatalf("fetched = %d, want 1", n)
	}
	f, err := s.Find(blob.Sum(data))
	if err != nil {
		t.Fatalf("Find after pull: %v", err)
	}
	if f.Label != "alliance" {
		t.Fatalf("Label = %q, want alliance", f.Label)
	}
}

func TestPullSkipsFramesAlreadyOnDisk(t *testing.T) {
	ctx := context.Background()
	bs := newMemBlob()
	data := []byte("frame-bytes")
	if _, _, err := blob.PutContent(ctx, bs, data); err != nil {
		t.Fatalf("seeding blob: %v", err)
	}
	s := corpus.New(t.TempDir())
	if _, _, err := s.Add("alliance", data); err != nil {
		t.Fatalf("Add: %v", err)
	}
	idx := corpus.Index{Frames: []corpus.Entry{{Hash: blob.Sum(data), Label: "alliance"}}}

	n, err := corpus.Pull(ctx, s, bs, idx)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if n != 0 {
		t.Fatalf("fetched = %d, want 0", n)
	}
}

// A frame named in the index but absent from the store is a real problem —
// someone committed an index without pushing. Say so rather than producing a
// silently short corpus that fails the gate for a mysterious reason.
func TestPullFailsLoudlyWhenABlobIsMissing(t *testing.T) {
	ctx := context.Background()
	s := corpus.New(t.TempDir())
	idx := corpus.Index{Frames: []corpus.Entry{{Hash: "beef", Label: "alliance"}}}

	if _, err := corpus.Pull(ctx, s, newMemBlob(), idx); err == nil {
		t.Fatal("Pull succeeded despite a missing blob")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/corpus/... -run 'Push|Pull'`
Expected: FAIL — `undefined: corpus.Push`, `undefined: corpus.Pull`.

- [ ] **Step 3: Write the implementation**

Create `internal/corpus/sync.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/corpus/... -v`
Expected: PASS.

- [ ] **Step 5: Run the full suite**

Run: `make test && make lint`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/corpus/sync.go internal/corpus/sync_test.go
git commit -m "Add corpus push/pull against the blob store

200+ full-resolution screenshots is too much for git history, so the bytes
live in the content-addressed blob store and only index.yaml is committed.
A frame in the index with no blob behind it fails loudly rather than
yielding a short corpus that fails the gate for no visible reason."
```

---

## Task 4: Record loop and the exported jitter helper

**Files:**
- Create: `internal/corpus/record.go`
- Test: `internal/corpus/record_test.go`
- Modify: `internal/runtime/jitter.go`, `internal/runtime/ctx.go:201`, `internal/runtime/act.go:93`, `internal/runtime/jitter_test.go`

**Interfaces:**
- Consumes: `Store.Add`, `Meta` from Tasks 1–2.
- Produces:
  - `type FrameSource interface { Frame(ctx context.Context) ([]byte, error) }`
  - `type RecordOptions struct { Count int; Sleep func(context.Context) error; Meta Meta }`
  - `type RecordResult struct { Captured, Duplicates int; Metas map[string]Meta }`
  - `func Record(ctx context.Context, s *Store, src FrameSource, opts RecordOptions) (RecordResult, error)`
  - `func runtime.Jitter(r *rand.Rand, min, max time.Duration) time.Duration` (renamed from private `jitter`)

**Why `Sleep` is injected rather than `corpus` importing `internal/runtime`:**
`internal/runtime` pulls in db, vision and transport. `internal/corpus` is
meant to stay a leaf that needs nothing but the filesystem, so the caller
supplies the jittered sleep. Invariant #7 is satisfied where it matters — at
the call site in `cmd/agent`, which uses the exported `runtime.Jitter` — while
the loop itself stays testable with an instant sleep.

**Why `FrameSource` rather than `transport.Transport`:** the same reason.
`Transport` yields `image.Image`; the corpus stores bytes. The PNG encode is
the caller's job, and a one-method interface makes the loop testable with a
slice of canned frames.

- [ ] **Step 1: Write the failing tests**

Create `internal/corpus/record_test.go`:

```go
package corpus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
)

// sliceSource yields canned frames in order, then reports exhaustion.
type sliceSource struct {
	frames [][]byte
	i      int
	err    error
}

func (s *sliceSource) Frame(context.Context) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.i >= len(s.frames) {
		return nil, errors.New("source exhausted")
	}
	f := s.frames[s.i]
	s.i++
	return f, nil
}

func noSleep(context.Context) error { return nil }

func TestRecordStoresFramesUnsortedAndCountsDuplicates(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{frames: [][]byte{[]byte("a"), []byte("a"), []byte("b")}}

	res, err := corpus.Record(context.Background(), s, src, corpus.RecordOptions{
		Count: 3,
		Sleep: noSleep,
		Meta:  corpus.Meta{Width: 1080, Height: 2400, Device: "Pixel 8a"},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if res.Captured != 2 {
		t.Fatalf("Captured = %d, want 2", res.Captured)
	}
	if res.Duplicates != 1 {
		t.Fatalf("Duplicates = %d, want 1", res.Duplicates)
	}

	frames, err := s.List(corpus.Unsorted)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("stored %d frames, want 2", len(frames))
	}
}

// The metadata a PNG cannot carry has to come back out of the run so the
// caller can fold it into the index.
func TestRecordReturnsMetaKeyedByHashForCapturedFramesOnly(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{frames: [][]byte{[]byte("a"), []byte("a")}}

	res, err := corpus.Record(context.Background(), s, src, corpus.RecordOptions{
		Count: 2,
		Sleep: noSleep,
		Meta:  corpus.Meta{Device: "Pixel 8a", GameVersion: "3.2.1"},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(res.Metas) != 1 {
		t.Fatalf("Metas = %+v, want one entry", res.Metas)
	}
	for _, m := range res.Metas {
		if m.Device != "Pixel 8a" || m.GameVersion != "3.2.1" {
			t.Fatalf("meta = %+v, want the run's device and game version", m)
		}
	}
}

// Ctrl-C during a ten-minute capture session must keep what it captured.
func TestRecordStopsOnContextCancellationWithoutLosingFrames(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{frames: [][]byte{[]byte("a"), []byte("b"), []byte("c")}}
	ctx, cancel := context.WithCancel(context.Background())

	sleepThenCancel := func(context.Context) error {
		cancel()
		return context.Canceled
	}

	res, err := corpus.Record(ctx, s, src, corpus.RecordOptions{
		Count: 0, // until the context ends
		Sleep: sleepThenCancel,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if res.Captured != 1 {
		t.Fatalf("Captured = %d, want the one frame taken before cancellation", res.Captured)
	}
}

func TestRecordFailsLoudlyOnTheFirstFrame(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{err: errors.New("no device")}

	if _, err := corpus.Record(context.Background(), s, src, corpus.RecordOptions{
		Count: 3,
		Sleep: noSleep,
	}); err == nil {
		t.Fatal("Record succeeded with a failing source")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/corpus/... -run Record`
Expected: FAIL — `undefined: corpus.Record`.

- [ ] **Step 3: Write the implementation**

Create `internal/corpus/record.go`:

```go
package corpus

import (
	"context"
	"errors"
	"fmt"
)

// FrameSource yields one encoded frame at a time. It exists so the record
// loop can be tested with canned bytes: transport.Transport yields an
// image.Image, and turning that into PNG bytes is the caller's job.
type FrameSource interface {
	Frame(ctx context.Context) ([]byte, error)
}

// RecordOptions configures one recording session.
type RecordOptions struct {
	// Count caps the number of source reads. Zero means keep going until the
	// context ends.
	Count int
	// Sleep waits between frames. It is injected rather than taken from
	// internal/runtime so this package stays a filesystem leaf; the caller
	// supplies a jittered sleep and satisfies invariant #7 there.
	Sleep func(ctx context.Context) error
	// Meta is stamped on every frame captured during this run. It cannot be
	// recovered from a PNG's bytes, so it has to be carried in.
	Meta Meta
}

// RecordResult reports what a session produced.
type RecordResult struct {
	Captured   int
	Duplicates int
	Metas      map[string]Meta
}

// Record captures frames from src into the corpus under Unsorted until the
// context ends or Count frames have been read.
//
// Duplicates are counted but not stored: a burst capture of an idle screen
// yields the same bytes repeatedly, and keeping them would skew the accuracy
// denominator. A failure on the very first frame is returned, because that is
// almost always "no device attached" and an empty corpus with a zero exit
// status is the worst possible outcome. Later failures end the session with
// what was already captured intact.
func Record(ctx context.Context, s *Store, src FrameSource, opts RecordOptions) (RecordResult, error) {
	res := RecordResult{Metas: map[string]Meta{}}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = func(context.Context) error { return nil }
	}

	for i := 0; opts.Count == 0 || i < opts.Count; i++ {
		if err := ctx.Err(); err != nil {
			return res, nil
		}

		data, err := src.Frame(ctx)
		if err != nil {
			if i == 0 {
				return res, fmt.Errorf("corpus: capturing the first frame: %w", err)
			}
			return res, nil
		}

		f, added, err := s.Add(Unsorted, data)
		if err != nil {
			return res, err
		}
		if !added {
			res.Duplicates++
		} else {
			res.Captured++
			res.Metas[f.Hash] = opts.Meta
		}

		if err := sleep(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return res, nil
			}
			return res, err
		}
	}
	return res, nil
}
```

- [ ] **Step 4: Export the jitter helper**

In `internal/runtime/jitter.go`, rename `jitter` to `Jitter` and update the
doc comment's first word:

```go
// Jitter draws a uniform duration in [min, max]. Fixed timing is the most
// detectable signal the platform emits, so every wait funnels through here
// (invariant #7). It is exported so callers outside the task runtime — the
// corpus recorder, for one — can comply without constructing a Ctx.
// Inverted or equal bounds collapse to min rather than panicking: a
// misconfigured range should still humanize, not crash mid-task.
func Jitter(r *rand.Rand, min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(r.Int63n(int64(max-min)+1))
}
```

Then update the four call sites:

```bash
sed -i 's/\bjitter(/Jitter(/g' internal/runtime/ctx.go internal/runtime/act.go internal/runtime/jitter_test.go
```

Verify the result touched exactly `ctx.go:201`, `act.go:93` and three lines in
`jitter_test.go`, and that `jitterPoint`/`jitterNorm` were **not** renamed:

```bash
git diff --stat internal/runtime/
grep -n 'jitterPoint\|jitterNorm' internal/runtime/act.go
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/corpus/... ./internal/runtime/... -v`
Expected: PASS.

- [ ] **Step 6: Run the full suite**

Run: `make test && make lint`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/corpus/record.go internal/corpus/record_test.go internal/runtime/
git commit -m "Add corpus record loop; export runtime.Jitter

The loop takes a one-method FrameSource and an injected sleep so
internal/corpus stays a filesystem leaf and the session is testable with
canned bytes. Jitter is exported so the recorder can satisfy invariant #7
without constructing a task Ctx.

A failure on the first frame is fatal because that is almost always 'no
device'; an empty corpus with a zero exit status is the worst outcome."
```

---

## Task 5: `agent record` and `agent corpus`

**Files:**
- Create: `cmd/agent/record.go`, `cmd/agent/corpus.go`
- Modify: `internal/transport/adb.go` (add `DeviceProps`), `cmd/agent/main.go`, `.gitignore`
- Test: `internal/transport/adb_props_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–4; `transport.NewADBTransport`, `transport.ListDevices`, `blob.New`, `config.Load`, `resolveSerial` (already in `cmd/agent/main.go:155`).
- Produces:
  - `func transport.DeviceProps(ctx context.Context, adbPath, serial, pkg string) (model, gameVersion string, err error)`
  - `func transport.ParseVersionName(dumpsys string) string`
  - `agent record --serial S --interval 2s --count N --duration D --corpus DIR --package PKG`
  - `agent corpus index|push|pull [--corpus DIR]`

**Why `DeviceProps` is a package function, not a `Transport` method:** the
`Transport` interface is deliberately narrow — pixels in, touches out. Adding
a general shell escape hatch to it would give every caller a way around
invariant #1. A free function in the same package reaches adb without widening
the interface every implementation must satisfy.

- [ ] **Step 1: Write the failing test for version parsing**

Only the parsing is unit-testable without a device; the adb call itself is
covered by the existing `device`-tagged suite. Create
`internal/transport/adb_props_test.go`:

```go
package transport_test

import (
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

func TestParseVersionName(t *testing.T) {
	const dump = `
Packages:
  Package [com.fun.lastwar.gp] (a1b2c3):
    userId=10234
    versionCode=3210 minSdk=24 targetSdk=34
    versionName=3.2.1
    splits=[base]
`
	if got := transport.ParseVersionName(dump); got != "3.2.1" {
		t.Fatalf("ParseVersionName = %q, want 3.2.1", got)
	}
}

func TestParseVersionNameTakesTheFirstOfSeveral(t *testing.T) {
	const dump = "    versionName=3.2.1\n    versionName=0.0.0\n"
	if got := transport.ParseVersionName(dump); got != "3.2.1" {
		t.Fatalf("ParseVersionName = %q, want 3.2.1", got)
	}
}

// An unknown version must not become a misleading empty-but-plausible value.
func TestParseVersionNameOnGarbageIsEmpty(t *testing.T) {
	if got := transport.ParseVersionName("no version here"); got != "" {
		t.Fatalf("ParseVersionName = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/transport/... -run ParseVersionName`
Expected: FAIL — `undefined: transport.ParseVersionName`.

- [ ] **Step 3: Implement `DeviceProps` and `ParseVersionName`**

Append to `internal/transport/adb.go`:

```go
// ParseVersionName pulls versionName out of `dumpsys package` output. It
// returns the first occurrence: a package with several split APKs repeats the
// field, and the first is the base.
func ParseVersionName(dumpsys string) string {
	const key = "versionName="
	for _, line := range strings.Split(dumpsys, "\n") {
		i := strings.Index(line, key)
		if i < 0 {
			continue
		}
		return strings.TrimSpace(line[i+len(key):])
	}
	return ""
}

// DeviceProps reads the device model and the installed game's version.
//
// This is a package function rather than a Transport method on purpose. The
// interface is deliberately narrow — pixels in, touches out — and adding a
// general shell escape hatch to it would hand every caller a way around
// invariant #1.
//
// Neither value is essential to capturing, so a failure to read one yields an
// empty string rather than an error: an unknown game version is worth
// recording as unknown, and is not a reason to abandon a capture session.
func DeviceProps(ctx context.Context, adbPath, serial, pkg string) (model, gameVersion string, err error) {
	args := func(rest ...string) []string {
		return append([]string{"-s", serial}, rest...)
	}

	out, err := exec.CommandContext(ctx, adbPath, args("shell", "getprop", "ro.product.model")...).Output()
	if err != nil {
		return "", "", fmt.Errorf("transport: reading model from %s: %w", serial, err)
	}
	model = strings.TrimSpace(string(out))

	dump, err := exec.CommandContext(ctx, adbPath, args("shell", "dumpsys", "package", pkg)...).Output()
	if err != nil {
		// The game may not be installed on a device used only for capture.
		return model, "", nil
	}
	return model, ParseVersionName(string(dump)), nil
}
```

Confirm `os/exec`, `strings`, `fmt` and `context` are already imported in
`adb.go`; add any that are missing.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/transport/... -run ParseVersionName -v`
Expected: PASS, three tests.

- [ ] **Step 5: Write `agent record`**

Create `cmd/agent/record.go`:

```go
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image/png"
	"math/rand"
	"time"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
)

// pngSource adapts a Transport to corpus.FrameSource: the transport yields an
// image.Image, and the corpus stores encoded bytes.
type pngSource struct{ tr transport.Transport }

func (s pngSource) Frame(ctx context.Context) ([]byte, error) {
	img, err := s.tr.Screenshot(ctx)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encoding frame: %w", err)
	}
	return buf.Bytes(), nil
}

func runRecord(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	serial := fs.String("serial", "", "device serial; optional when exactly one device is attached")
	root := fs.String("corpus", "fixtures/corpus", "corpus root directory")
	interval := fs.Duration("interval", 2*time.Second, "nominal wait between frames; jittered ±40%")
	count := fs.Int("count", 0, "stop after N frames; 0 means run until --duration or Ctrl-C")
	duration := fs.Duration("duration", 0, "stop after this long; 0 means run until --count or Ctrl-C")
	pkg := fs.String("package", transport.DefaultPackage, "game package name, for the version stamp")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolved, err := resolveSerial(ctx, cfg.ADBPath, *serial)
	if err != nil {
		return err
	}
	tr, err := transport.NewADBTransport(ctx, transport.ADBOptions{
		ADBPath: cfg.ADBPath,
		Serial:  resolved,
		Package: *pkg,
	})
	if err != nil {
		return fmt.Errorf("opening device %s: %w", resolved, err)
	}
	defer tr.Close()

	model, gameVersion, err := transport.DeviceProps(ctx, cfg.ADBPath, resolved, *pkg)
	if err != nil {
		return err
	}
	res := tr.Resolution()

	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	// Jitter the wait rather than sleeping a fixed interval. Recording is
	// human-driven, but fixed timing is the most detectable signal we emit
	// and there is no reason for this loop to be the exception (invariant #7).
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	low := time.Duration(float64(*interval) * 0.6)
	high := time.Duration(float64(*interval) * 1.4)
	sleep := func(ctx context.Context) error {
		t := time.NewTimer(runtime.Jitter(rng, low, high))
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}

	store := corpus.New(*root)
	result, err := corpus.Record(ctx, store, pngSource{tr: tr}, corpus.RecordOptions{
		Count: *count,
		Sleep: sleep,
		Meta: corpus.Meta{
			Width:       res.X,
			Height:      res.Y,
			CapturedAt:  time.Now().UTC(),
			Device:      model,
			GameVersion: gameVersion,
		},
	})
	if err != nil {
		return err
	}

	// Fold this session's metadata into the index straight away, so a
	// forgotten `corpus index` cannot lose the device and version stamps.
	prev, err := store.ReadIndex()
	if err != nil {
		return err
	}
	frames, err := store.All()
	if err != nil {
		return err
	}
	if err := store.WriteIndex(corpus.Reindex(prev, frames, result.Metas)); err != nil {
		return err
	}

	fmt.Printf("captured=%d duplicates=%d corpus=%s device=%s resolution=%dx%d game_version=%s\n",
		result.Captured, result.Duplicates, *root, model, res.X, res.Y, gameVersion)
	return nil
}
```

- [ ] **Step 6: Write `agent corpus`**

Create `cmd/agent/corpus.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"

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
		prev, err := store.ReadIndex()
		if err != nil {
			return err
		}
		frames, err := store.All()
		if err != nil {
			return err
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
```

Add `"os"` to the import block.

- [ ] **Step 7: Wire the subcommands into `main.go`**

In `cmd/agent/main.go`, add to the `usage()` string after the `run` line:

```
  record    burst-capture frames into the fixture corpus
  corpus    manage the fixture corpus: index, push, pull
```

and to the `switch os.Args[1]` block after `case "run":`:

```go
	case "record":
		return runRecord(ctx, cfg, os.Args[2:])
	case "corpus":
		return runCorpus(ctx, cfg, os.Args[2:])
```

- [ ] **Step 8: Ignore the corpus bytes, keep the index**

Append to `.gitignore`:

```gitignore
# Corpus frames live in the blob store; only the index is committed.
fixtures/corpus/*
!fixtures/corpus/index.yaml
```

- [ ] **Step 9: Verify it builds and the suite passes**

Run: `make build && make test && make lint && make verify-nocgo`
Expected: PASS. Then confirm the commands are registered:

```bash
./bin/agent help 2>&1 | grep -E 'record|corpus'
./bin/agent corpus 2>&1 | head -5
```
Expected: both new commands listed; `agent corpus` prints its usage and exits non-zero.

- [ ] **Step 10: Commit**

```bash
git add cmd/agent/record.go cmd/agent/corpus.go cmd/agent/main.go \
        internal/transport/adb.go internal/transport/adb_props_test.go .gitignore
git commit -m "Add agent record and agent corpus commands

record bursts frames into _unsorted/ while the operator drives the phone by
hand, stamping device model and game version into the index as it goes.
DeviceProps is a package function rather than a Transport method: the
interface is narrow on purpose, and a shell escape hatch on it would be a
way around invariant #1.

Corpus bytes are gitignored and live in the blob store; index.yaml is the
committed projection."
```

---

## Task 6: Manifest writer with revalidate-and-rollback

**Files:**
- Create: `internal/vision/manifest_write.go`
- Test: `internal/vision/manifest_write_test.go`

**Interfaces:**
- Consumes: the private `manifestFile`, `screenManifest`, `anchorManifest` types and `LoadRegistry` in `internal/vision/registry.go`; `transport.Rect`.
- Produces:
  - `type AnchorSpec struct { Screen, ID string; Region transport.Rect; Threshold float64; IdentifiesScreen bool }`
  - `func WriteAnchor(manifestPath string, refHeight int, spec AnchorSpec, pngBytes []byte) error`
  - `func SetThresholds(manifestPath string, thresholds map[string]float64) error`

`SetThresholds` is keyed `"<screen>/<anchorID>"`. Task 12 (`agent score
--apply-thresholds`) is its only caller; it is written here because it shares
the write-revalidate-rollback machinery and testing both together is cheaper
than testing them apart.

**The design point:** `LoadRegistry` already validates loudly — inverted
region, out-of-range threshold, missing template file. Reusing it as a
*write-time* check means the manifest can never be left in a state that breaks
`agent run-task` later. The rollback is what makes that true rather than
merely likely: a half-written manifest that fails validation is worse than no
write at all, because the failure surfaces hours later on a different command.

- [ ] **Step 1: Write the failing tests**

Create `internal/vision/manifest_write_test.go`:

```go
package vision_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// tinyPNG returns a valid 4x4 PNG so template loading succeeds.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8(x * 60)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding test PNG: %v", err)
	}
	return buf.Bytes()
}

func spec(screen, id string) vision.AnchorSpec {
	return vision.AnchorSpec{
		Screen:           screen,
		ID:               id,
		Region:           transport.Rect{X1: 0.1, Y1: 0.1, X2: 0.4, Y2: 0.3},
		Threshold:        0.85,
		IdentifiesScreen: true,
	}
}

func TestWriteAnchorCreatesTheManifestAndTemplate(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")

	if err := vision.WriteAnchor(manifest, 2400, spec("alliance", "alliance_button"), tinyPNG(t)); err != nil {
		t.Fatalf("WriteAnchor: %v", err)
	}

	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry after write: %v", err)
	}
	if reg.ReferenceHeight != 2400 {
		t.Fatalf("ReferenceHeight = %d, want 2400", reg.ReferenceHeight)
	}
	s, ok := reg.Screen("alliance")
	if !ok {
		t.Fatal("screen alliance missing from the registry")
	}
	if len(s.Anchors) != 1 || s.Anchors[0].ID != "alliance_button" {
		t.Fatalf("anchors = %+v, want one alliance_button", s.Anchors)
	}
	if !s.Anchors[0].IdentifiesScreen {
		t.Fatal("IdentifiesScreen was not persisted")
	}
}

func TestWriteAnchorReplacesAnAnchorWithTheSameID(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	if err := vision.WriteAnchor(manifest, 2400, spec("alliance", "alliance_button"), tinyPNG(t)); err != nil {
		t.Fatalf("first WriteAnchor: %v", err)
	}

	updated := spec("alliance", "alliance_button")
	updated.Threshold = 0.7
	if err := vision.WriteAnchor(manifest, 2400, updated, tinyPNG(t)); err != nil {
		t.Fatalf("second WriteAnchor: %v", err)
	}

	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	s, _ := reg.Screen("alliance")
	if len(s.Anchors) != 1 {
		t.Fatalf("anchors = %+v, want the anchor replaced, not duplicated", s.Anchors)
	}
	if s.Anchors[0].Threshold != 0.7 {
		t.Fatalf("Threshold = %v, want 0.7", s.Anchors[0].Threshold)
	}
}

// A half-written manifest that fails validation is worse than no write: the
// failure would surface hours later, on a different command.
func TestWriteAnchorRollsBackWhenTheResultWouldNotLoad(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	good := spec("alliance", "alliance_button")
	if err := vision.WriteAnchor(manifest, 2400, good, tinyPNG(t)); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}

	bad := spec("alliance", "broken")
	bad.Region = transport.Rect{X1: 0.9, Y1: 0.9, X2: 0.1, Y2: 0.1} // inverted

	if err := vision.WriteAnchor(manifest, 2400, bad, tinyPNG(t)); err == nil {
		t.Fatal("WriteAnchor accepted an inverted region")
	}

	after, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading manifest after rollback: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("manifest changed despite a failed write:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(dir, "alliance", "broken.png")); !os.IsNotExist(err) {
		t.Fatal("the rejected template PNG was left behind")
	}
	if _, err := vision.LoadRegistry(manifest); err != nil {
		t.Fatalf("registry no longer loads after a rolled-back write: %v", err)
	}
}

func TestWriteAnchorRejectsAChangeOfReferenceHeight(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	if err := vision.WriteAnchor(manifest, 2400, spec("alliance", "a"), tinyPNG(t)); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := vision.WriteAnchor(manifest, 1920, spec("mail", "b"), tinyPNG(t)); err == nil {
		t.Fatal("WriteAnchor silently mixed templates captured at two reference heights")
	}
}

func TestSetThresholdsUpdatesNamedAnchorsOnly(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	for _, s := range []vision.AnchorSpec{spec("alliance", "a"), spec("mail", "b")} {
		if err := vision.WriteAnchor(manifest, 2400, s, tinyPNG(t)); err != nil {
			t.Fatalf("seeding %s/%s: %v", s.Screen, s.ID, err)
		}
	}

	if err := vision.SetThresholds(manifest, map[string]float64{"alliance/a": 0.62}); err != nil {
		t.Fatalf("SetThresholds: %v", err)
	}

	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	alliance, _ := reg.Screen("alliance")
	if alliance.Anchors[0].Threshold != 0.62 {
		t.Fatalf("alliance/a threshold = %v, want 0.62", alliance.Anchors[0].Threshold)
	}
	mail, _ := reg.Screen("mail")
	if mail.Anchors[0].Threshold != 0.85 {
		t.Fatalf("mail/b threshold = %v, want 0.85 unchanged", mail.Anchors[0].Threshold)
	}
}

func TestSetThresholdsRejectsAnUnknownAnchor(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	if err := vision.WriteAnchor(manifest, 2400, spec("alliance", "a"), tinyPNG(t)); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := vision.SetThresholds(manifest, map[string]float64{"alliance/nope": 0.5}); err == nil {
		t.Fatal("SetThresholds silently ignored an anchor that does not exist")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/vision/... -run 'WriteAnchor|SetThresholds'`
Expected: FAIL — `undefined: vision.WriteAnchor`, `undefined: vision.AnchorSpec`.

- [ ] **Step 3: Write the implementation**

Create `internal/vision/manifest_write.go`:

```go
package vision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	yaml "go.yaml.in/yaml/v3"

	"github.com/tomharris/lw-manager/internal/transport"
)

// AnchorSpec describes an anchor to add to or replace in the manifest.
type AnchorSpec struct {
	Screen           string
	ID               string
	Region           transport.Rect
	Threshold        float64
	IdentifiesScreen bool
}

// WriteAnchor adds or replaces an anchor, writing its template PNG alongside
// the manifest.
//
// It re-runs LoadRegistry afterwards and rolls both writes back if the result
// no longer loads. LoadRegistry already validates loudly; reusing it as a
// write-time check is what keeps the manifest from ever reaching a state that
// breaks `agent run-task`. Without the rollback that would be merely likely
// rather than true: a half-written manifest surfaces hours later, on a
// different command, with no obvious cause.
func WriteAnchor(manifestPath string, refHeight int, spec AnchorSpec, pngBytes []byte) error {
	if spec.Screen == "" || spec.ID == "" {
		return fmt.Errorf("vision: anchor needs both a screen and an id, got %q/%q", spec.Screen, spec.ID)
	}
	if !spec.Region.Valid() {
		return fmt.Errorf("vision: anchor %s/%s region %+v is not a valid unit-square rect",
			spec.Screen, spec.ID, spec.Region)
	}
	if spec.Threshold < 0 || spec.Threshold > 1 {
		return fmt.Errorf("vision: anchor %s/%s threshold %.3f outside [0,1]",
			spec.Screen, spec.ID, spec.Threshold)
	}

	mf, prevManifest, err := readManifest(manifestPath)
	if err != nil {
		return err
	}
	if mf.ReferenceHeight == 0 {
		mf.ReferenceHeight = refHeight
	} else if mf.ReferenceHeight != refHeight {
		// One template library, one capture resolution. Mixing two silently
		// mis-scales every match, and the symptom is a recognizer that is
		// merely bad rather than obviously broken.
		return fmt.Errorf("vision: manifest %s was captured at reference height %d, refusing to add a template at %d",
			manifestPath, mf.ReferenceHeight, refHeight)
	}

	dir := filepath.Dir(manifestPath)
	rel := filepath.Join(spec.Screen, spec.ID+".png")
	tmplPath := filepath.Join(dir, rel)

	prevTemplate, hadTemplate, err := readIfExists(tmplPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(tmplPath), 0o755); err != nil {
		return fmt.Errorf("vision: creating %s: %w", filepath.Dir(tmplPath), err)
	}
	if err := os.WriteFile(tmplPath, pngBytes, 0o644); err != nil {
		return fmt.Errorf("vision: writing template %s: %w", tmplPath, err)
	}

	if mf.Screens == nil {
		mf.Screens = map[string]screenManifest{}
	}
	sm := mf.Screens[spec.Screen]
	entry := anchorManifest{
		ID:               spec.ID,
		Template:         filepath.ToSlash(rel),
		Region:           [4]float64{spec.Region.X1, spec.Region.Y1, spec.Region.X2, spec.Region.Y2},
		Threshold:        spec.Threshold,
		IdentifiesScreen: spec.IdentifiesScreen,
	}
	replaced := false
	for i := range sm.Anchors {
		if sm.Anchors[i].ID == spec.ID {
			sm.Anchors[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		sm.Anchors = append(sm.Anchors, entry)
	}
	sort.Slice(sm.Anchors, func(i, j int) bool { return sm.Anchors[i].ID < sm.Anchors[j].ID })
	mf.Screens[spec.Screen] = sm

	rollback := func() {
		restore(manifestPath, prevManifest, len(prevManifest) > 0)
		restore(tmplPath, prevTemplate, hadTemplate)
	}
	if err := writeManifest(manifestPath, mf); err != nil {
		rollback()
		return err
	}
	if _, err := LoadRegistry(manifestPath); err != nil {
		rollback()
		return fmt.Errorf("vision: anchor %s/%s would break the manifest, rolled back: %w",
			spec.Screen, spec.ID, err)
	}
	return nil
}

// SetThresholds updates thresholds for anchors keyed "<screen>/<anchorID>".
//
// An unknown key is an error rather than a no-op: the only caller is
// `agent score --apply-thresholds`, and silently skipping a key there would
// report success while leaving the anchor that actually needs fixing alone.
func SetThresholds(manifestPath string, thresholds map[string]float64) error {
	mf, prevManifest, err := readManifest(manifestPath)
	if err != nil {
		return err
	}

	applied := map[string]bool{}
	for name, sm := range mf.Screens {
		for i := range sm.Anchors {
			key := name + "/" + sm.Anchors[i].ID
			v, ok := thresholds[key]
			if !ok {
				continue
			}
			if v < 0 || v > 1 {
				return fmt.Errorf("vision: threshold %.3f for %s outside [0,1]", v, key)
			}
			sm.Anchors[i].Threshold = v
			applied[key] = true
		}
		mf.Screens[name] = sm
	}
	for key := range thresholds {
		if !applied[key] {
			return fmt.Errorf("vision: manifest %s has no anchor %q", manifestPath, key)
		}
	}

	if err := writeManifest(manifestPath, mf); err != nil {
		restore(manifestPath, prevManifest, len(prevManifest) > 0)
		return err
	}
	if _, err := LoadRegistry(manifestPath); err != nil {
		restore(manifestPath, prevManifest, len(prevManifest) > 0)
		return fmt.Errorf("vision: threshold update would break the manifest, rolled back: %w", err)
	}
	return nil
}

// readManifest loads the manifest for editing, returning its raw bytes so a
// failed write can be rolled back exactly. A missing file is an empty
// manifest: the first crop creates it.
func readManifest(path string) (manifestFile, []byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return manifestFile{}, nil, nil
	}
	if err != nil {
		return manifestFile{}, nil, fmt.Errorf("vision: reading manifest %s: %w", path, err)
	}
	var mf manifestFile
	if err := yaml.Unmarshal(raw, &mf); err != nil {
		return manifestFile{}, nil, fmt.Errorf("vision: parsing manifest %s: %w", path, err)
	}
	return mf, raw, nil
}

func writeManifest(path string, mf manifestFile) error {
	raw, err := yaml.Marshal(mf)
	if err != nil {
		return fmt.Errorf("vision: encoding manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("vision: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("vision: writing manifest %s: %w", path, err)
	}
	return nil
}

func readIfExists(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("vision: reading %s: %w", path, err)
	}
	return raw, true, nil
}

// restore puts a file back the way it was, deleting it when it did not exist
// before. Errors here are unrecoverable and deliberately ignored: the caller
// is already returning the failure that triggered the rollback, and masking
// it with a rollback error would hide the real cause.
func restore(path string, data []byte, existed bool) {
	if existed {
		_ = os.WriteFile(path, data, 0o644)
		return
	}
	_ = os.Remove(path)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/vision/... -run 'WriteAnchor|SetThresholds' -v`
Expected: PASS, six tests.

- [ ] **Step 5: Run the full suite**

Run: `make test && make lint`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/vision/manifest_write.go internal/vision/manifest_write_test.go
git commit -m "Add manifest writer that revalidates and rolls back

LoadRegistry already validates loudly, so reusing it as a write-time check
keeps the manifest from ever reaching a state that breaks agent run-task.
The rollback is what makes that true rather than merely likely: a
half-written manifest surfaces hours later on a different command.

Refuses to mix two reference heights in one library, because that
mis-scales every match and reads as a merely-bad recognizer rather than an
obviously broken one."
```

---

## Task 7: Studio server, token gate, frame serving

**Files:**
- Create: `internal/studio/studio.go`, `internal/studio/studio_test.go`

**Interfaces:**
- Consumes: `corpus.Store`, `corpus.Frame`, `corpus.Unsorted` (Task 1); `transport.Transport`.
- Produces:
  - `type Options struct { Corpus *corpus.Store; Transport transport.Transport; ManifestPath string; RefHeight int; Token string; Logger *slog.Logger }`
  - `type Server struct{ ... }`
  - `func New(opts Options) (*Server, error)`
  - `func (s *Server) Handler() http.Handler`
  - `func RequireToken(addr string) bool`
  - `var ErrTokenRequired error`

**Why the token check lives in `New` and not the CLI:** binding to the LAN
without auth is the kind of mistake that is silent until it is not. `New`
therefore requires a token *unconditionally* — stricter than the spec's
"non-loopback needs a token", and stricter on purpose, because a rule with an
exception is a rule someone routes around. `RequireToken` is a separate pure
predicate used by the CLI to warn that this bind is reachable from the
network; it does not gate anything, because `New` already did.

- [ ] **Step 1: Write the failing tests**

Create `internal/studio/studio_test.go`:

```go
package studio_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/studio"
)

func newTestServer(t *testing.T, token string) (*studio.Server, *corpus.Store) {
	t.Helper()
	store := corpus.New(t.TempDir())
	srv, err := studio.New(studio.Options{
		Corpus:       store,
		ManifestPath: t.TempDir() + "/manifest.yaml",
		RefHeight:    2400,
		Token:        token,
	})
	if err != nil {
		t.Fatalf("studio.New: %v", err)
	}
	return srv, store
}

func TestRequireTokenIsTrueOnlyForNonLoopbackBinds(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8088": false,
		"localhost:8088": false,
		"[::1]:8088":     false,
		"0.0.0.0:8088":   true,
		":8088":          true,
		"192.168.1.5:80": true,
	} {
		if got := studio.RequireToken(addr); got != want {
			t.Errorf("RequireToken(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestTokenInQueryStringSetsACookieAndAdmits(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?t=s3cret", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == studio.CookieName && c.Value == "s3cret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s cookie set; cookies = %v", studio.CookieName, rec.Result().Cookies())
	}
}

func TestCookieAdmitsAndAWrongTokenDoesNot(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	ok := httptest.NewRequest(http.MethodGet, "/", nil)
	ok.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	recOK := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOK, ok)
	if recOK.Code != http.StatusOK {
		t.Fatalf("valid cookie: status = %d, want 200", recOK.Code)
	}

	bad := httptest.NewRequest(http.MethodGet, "/", nil)
	bad.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "wrong"})
	recBad := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBad, bad)
	if recBad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: status = %d, want 401", recBad.Code)
	}
}

func TestNewRejectsAnEmptyToken(t *testing.T) {
	if _, err := studio.New(studio.Options{
		Corpus:       corpus.New(t.TempDir()),
		ManifestPath: "manifest.yaml",
		RefHeight:    2400,
	}); !errors.Is(err, studio.ErrTokenRequired) {
		t.Fatalf("New with no token: err = %v, want ErrTokenRequired", err)
	}
}

func TestFrameEndpointServesTheStoredBytes(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/frame/"+f.Hash, nil)
	req.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "frame-bytes" {
		t.Fatalf("body = %q, want the stored bytes", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
}

func TestFrameEndpointIs404ForAnUnknownHash(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/frame/deadbeef", nil)
	req.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/studio/...`
Expected: FAIL — `no Go files in .../internal/studio`.

- [ ] **Step 3: Write the implementation**

Create `internal/studio/studio.go`:

```go
// Package studio serves the corpus labelling and cropping UI.
//
// It exists because the build host is headless and driven over SSH: a browser
// on another machine is the only surface where a screenshot can actually be
// looked at, so both labelling and cropping have to live there. Threshold
// tuning deliberately does not — that is a batch report over the whole corpus
// (see `agent score`), because the number worth having is computed across
// hundreds of frames rather than eyeballed on one.
package studio

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/transport"
)

// CookieName carries the studio token between requests.
const CookieName = "lw_studio"

// ErrTokenRequired reports a server constructed without a token.
var ErrTokenRequired = errors.New("studio: a token is required")

// Options configures a studio server.
type Options struct {
	Corpus *corpus.Store
	// Transport backs "capture now". Optional: without it the button is
	// disabled rather than the server refusing to start, so the studio is
	// still usable for labelling a corpus on a machine with no phone.
	Transport    transport.Transport
	ManifestPath string
	RefHeight    int
	Token        string
	Logger       *slog.Logger
}

// Server serves the studio UI.
type Server struct {
	corpus    *corpus.Store
	tr        transport.Transport
	manifest  string
	refHeight int
	token     string
	log       *slog.Logger
}

// New validates options and builds a server.
//
// The token is mandatory here rather than at the CLI. Binding to the LAN
// without auth is silent until it is not, and enforcing it at construction
// means no caller can forget.
func New(opts Options) (*Server, error) {
	if opts.Corpus == nil {
		return nil, fmt.Errorf("studio: a corpus store is required")
	}
	if opts.Token == "" {
		return nil, ErrTokenRequired
	}
	if opts.ManifestPath == "" {
		return nil, fmt.Errorf("studio: a manifest path is required")
	}
	if opts.RefHeight <= 0 {
		return nil, fmt.Errorf("studio: reference height must be positive, got %d", opts.RefHeight)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		corpus:    opts.Corpus,
		tr:        opts.Transport,
		manifest:  opts.ManifestPath,
		refHeight: opts.RefHeight,
		token:     opts.Token,
		log:       log,
	}, nil
}

// RequireToken reports whether addr is non-loopback, and therefore must never
// be served without a token. A bare port or an empty host means every
// interface, which is the most exposed case, not the least.
func RequireToken(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // a hostname we cannot resolve to loopback: assume exposed
	}
	return !ip.IsLoopback()
}

// Handler returns the routed, token-gated handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleUnsorted)
	mux.HandleFunc("GET /labeled", s.handleLabeled)
	mux.HandleFunc("GET /crop", s.handleCropView)
	mux.HandleFunc("GET /frame/{hash}", s.handleFrame)
	mux.HandleFunc("POST /label", s.handleLabel)
	mux.HandleFunc("POST /capture", s.handleCapture)
	mux.HandleFunc("POST /crop", s.handleCrop)
	return s.authenticate(mux)
}

// authenticate admits a request carrying the token in a cookie, or in ?t= on
// a first visit, in which case it sets the cookie so later requests carry it.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t != "" && s.tokenOK(t) {
			http.SetCookie(w, &http.Cookie{
				Name:     CookieName,
				Value:    t,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(CookieName)
		if err != nil || !s.tokenOK(c.Value) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenOK compares in constant time. The studio is on a LAN, not the open
// internet, but a timing-safe compare costs nothing here.
func (s *Server) tokenOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	data, err := s.corpus.Read(r.PathValue("hash"))
	if errors.Is(err, corpus.ErrNotFound) {
		http.Error(w, "no such frame", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("studio: reading frame", "hash", r.PathValue("hash"), "err", err)
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(data); err != nil {
		s.log.Warn("studio: writing frame response", "err", err)
	}
}
```

The remaining handlers (`handleUnsorted`, `handleLabeled`, `handleCropView`,
`handleLabel`, `handleCapture`, `handleCrop`) arrive in Tasks 8 and 9. To keep
this task compiling and its tests meaningful, add temporary stubs at the
bottom of `studio.go` that Task 8 replaces:

```go
func (s *Server) handleUnsorted(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
func (s *Server) handleLabeled(w http.ResponseWriter, r *http.Request)  { w.WriteHeader(http.StatusOK) }
func (s *Server) handleCropView(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
func (s *Server) handleLabel(w http.ResponseWriter, r *http.Request)    { w.WriteHeader(http.StatusOK) }
func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request)  { w.WriteHeader(http.StatusOK) }
func (s *Server) handleCrop(w http.ResponseWriter, r *http.Request)     { w.WriteHeader(http.StatusOK) }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/studio/... -v`
Expected: PASS, seven tests.

- [ ] **Step 5: Commit**

```bash
git add internal/studio/studio.go internal/studio/studio_test.go
git commit -m "Add studio server with a mandatory token and frame serving

The token is enforced in New rather than at the CLI: binding to the LAN
without auth is silent until it is not, so no caller gets the chance to
forget. RequireToken stays a pure function of the address, which makes the
rule itself testable, and treats a bare port as the most exposed case."
```

---

## Task 8: Label grid, relabelling, capture-now

**Files:**
- Create: `internal/studio/views.go`, `internal/studio/handlers.go`, `internal/studio/handlers_test.go`
- Modify: `internal/studio/studio.go` (delete the four stubs this task replaces)

**Interfaces:**
- Consumes: `Server` fields from Task 7; `corpus.Store.List/Labels/Relabel/Add`; `transport.Transport.Screenshot`.
- Produces: `handleUnsorted`, `handleLabeled`, `handleLabel`, `handleCapture`, and `var KnownLabels []string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/studio/handlers_test.go`:

```go
package studio_test

import (
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/studio"
	"github.com/tomharris/lw-manager/internal/transport"
)

func authed(t *testing.T, method, target string, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	return req
}

func TestUnsortedGridListsEveryUnlabelledFrame(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodGet, "/", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), f.Hash) {
		t.Fatal("the unsorted frame's hash is not on the page")
	}
	// Every known label must be offerable, including the negatives bucket.
	for _, want := range []string{"alliance_members", "vs_ranking", corpus.None} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("label %q is not offered on the page", want)
		}
	}
}

func TestPostLabelMovesTheFrame(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{"hash": {f.Hash}, "label": {"alliance"}}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/label", form.Encode()))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	got, err := store.Find(f.Hash)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Label != "alliance" {
		t.Fatalf("Label = %q, want alliance", got.Label)
	}
}

// The label arrives from a browser form, so a hostile value must not escape
// the corpus root.
func TestPostLabelRejectsAnUnsafeLabel(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{"hash": {f.Hash}, "label": {"../../etc"}}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/label", form.Encode()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	got, err := store.Find(f.Hash)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Label != corpus.Unsorted {
		t.Fatalf("Label = %q, want the frame left alone", got.Label)
	}
}

func TestPostCaptureStoresAFreshFrame(t *testing.T) {
	store := corpus.New(t.TempDir())
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	tr, err := transport.NewReplayTransportFromImages(img)
	if err != nil {
		t.Fatalf("NewReplayTransportFromImages: %v", err)
	}
	srv, err := studio.New(studio.Options{
		Corpus:       store,
		Transport:    tr,
		ManifestPath: t.TempDir() + "/manifest.yaml",
		RefHeight:    2400,
		Token:        "s3cret",
	})
	if err != nil {
		t.Fatalf("studio.New: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/capture", ""))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	frames, err := store.List(corpus.Unsorted)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("stored %d frames, want 1", len(frames))
	}
}

// Labelling a corpus on a machine with no phone must still work.
func TestPostCaptureWithoutATransportIsRejectedNotFatal(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret") // constructed with no Transport

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/capture", ""))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestLabeledBrowserGroupsFramesByLabel(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	if _, _, err := store.Add("alliance", []byte("a")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, _, err := store.Add(corpus.None, []byte("b")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodGet, "/labeled", ""))

	body := rec.Body.String()
	if !strings.Contains(body, "alliance") || !strings.Contains(body, corpus.None) {
		t.Fatalf("labeled page missing a label group:\n%s", body)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/studio/... -run 'Unsorted|Label|Capture'`
Expected: FAIL — the stubs return 200 with an empty body, so the hash and
label assertions fail.

- [ ] **Step 3: Write the views**

Create `internal/studio/views.go`:

```go
package studio

import (
	"html/template"

	"github.com/tomharris/lw-manager/internal/corpus"
)

// KnownLabels is the screen set this corpus is built for.
//
// The first six are the screens DefaultGraph navigates. alliance_members and
// vs_ranking are here because the recognizer must be able to *name* every
// screen the corpus asserts exists — a labelled frame with no identifying
// anchor is wrong on every scoring run, forever. Adding graph edges to them
// is M4 capture-route work and is deliberately not part of this.
var KnownLabels = []string{
	"base",
	"world_map",
	"alliance",
	"alliance_tech",
	"alliance_members",
	"vs_ranking",
	"mail",
	"radar",
	corpus.None,
}

const layout = `
<!doctype html>
<meta charset="utf-8">
<title>{{.Title}} — lw studio</title>
<style>
 body{font:14px/1.5 system-ui,sans-serif;margin:1rem;background:#111;color:#eee}
 a{color:#8cf} nav a{margin-right:1rem}
 .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:1rem}
 .card{background:#1b1b1b;padding:.5rem;border-radius:6px}
 .card img{width:100%;height:auto;display:block;border-radius:4px}
 select,button{font:inherit;margin-top:.4rem;width:100%}
 h2{margin-top:2rem;border-bottom:1px solid #333}
 .count{color:#999}
</style>
<nav>
 <a href="/">unsorted</a><a href="/labeled">labeled</a>
 <form method="post" action="/capture" style="display:inline">
  <button style="width:auto" {{if not .CanCapture}}disabled title="no device attached"{{end}}>capture now</button>
 </form>
</nav>
`

var unsortedTmpl = template.Must(template.New("unsorted").Parse(layout + `
<h1>unsorted <span class="count">({{len .Frames}})</span></h1>
<div class="grid">
{{range .Frames}}
 <div class="card">
  <a href="/crop?hash={{.Hash}}"><img src="/frame/{{.Hash}}" alt="{{.Hash}}"></a>
  <form method="post" action="/label">
   <input type="hidden" name="hash" value="{{.Hash}}">
   <select name="label">{{range $.Labels}}<option value="{{.}}">{{.}}</option>{{end}}</select>
   <button>label</button>
  </form>
  <small>{{slice .Hash 0 12}}</small>
 </div>
{{end}}
</div>
`))

var labeledTmpl = template.Must(template.New("labeled").Parse(layout + `
<h1>labeled</h1>
{{range .Groups}}
 <h2>{{.Label}} <span class="count">({{len .Frames}})</span></h2>
 <div class="grid">
 {{range .Frames}}
  <div class="card">
   <a href="/crop?hash={{.Hash}}"><img src="/frame/{{.Hash}}" alt="{{.Hash}}"></a>
   <form method="post" action="/label">
    <input type="hidden" name="hash" value="{{.Hash}}">
    <select name="label">{{range $.Labels}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <button>move</button>
   </form>
  </div>
 {{end}}
 </div>
{{end}}
`))

// group is one label's frames on the labeled page.
type group struct {
	Label  string
	Frames []corpus.Frame
}
```

- [ ] **Step 4: Write the handlers**

Create `internal/studio/handlers.go`:

```go
package studio

import (
	"bytes"
	"errors"
	"image/png"
	"net/http"

	"github.com/tomharris/lw-manager/internal/corpus"
)

func (s *Server) handleUnsorted(w http.ResponseWriter, r *http.Request) {
	frames, err := s.corpus.List(corpus.Unsorted)
	if err != nil {
		s.fail(w, "listing unsorted frames", err)
		return
	}
	s.render(w, unsortedTmpl, map[string]any{
		"Title":      "unsorted",
		"Frames":     frames,
		"Labels":     KnownLabels,
		"CanCapture": s.tr != nil,
	})
}

func (s *Server) handleLabeled(w http.ResponseWriter, r *http.Request) {
	labels, err := s.corpus.Labels()
	if err != nil {
		s.fail(w, "listing labels", err)
		return
	}
	var groups []group
	for _, l := range labels {
		if l == corpus.Unsorted {
			continue
		}
		frames, err := s.corpus.List(l)
		if err != nil {
			s.fail(w, "listing frames for "+l, err)
			return
		}
		groups = append(groups, group{Label: l, Frames: frames})
	}
	s.render(w, labeledTmpl, map[string]any{
		"Title":      "labeled",
		"Groups":     groups,
		"Labels":     KnownLabels,
		"CanCapture": s.tr != nil,
	})
}

func (s *Server) handleLabel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	hash, label := r.FormValue("hash"), r.FormValue("label")

	// The label arrives from a browser form, so validate before it can become
	// a directory name.
	if err := corpus.CheckLabel(label); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch _, err := s.corpus.Relabel(hash, label); {
	case errors.Is(err, corpus.ErrNotFound):
		http.Error(w, "no such frame", http.StatusNotFound)
		return
	case err != nil:
		s.fail(w, "relabelling "+hash, err)
		return
	}
	http.Redirect(w, r, r.Header.Get("Referer")+"", http.StatusSeeOther)
}

// handleCapture grabs a frame from the device on demand. While cropping, the
// wanted screen is easier to produce on the handset than to hunt for in the
// corpus.
func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	if s.tr == nil {
		http.Error(w, "no device attached to this studio", http.StatusServiceUnavailable)
		return
	}
	img, err := s.tr.Screenshot(r.Context())
	if err != nil {
		s.fail(w, "capturing a frame", err)
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		s.fail(w, "encoding the captured frame", err)
		return
	}
	if _, _, err := s.corpus.Add(corpus.Unsorted, buf.Bytes()); err != nil {
		s.fail(w, "storing the captured frame", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) render(w http.ResponseWriter, t *template.Template, data any) {
	// Rendered into a buffer first so a template failure produces a 500
	// rather than a half-written page with a 200 already committed.
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		s.fail(w, "rendering", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.log.Warn("studio: writing response", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("studio: "+what, "err", err)
	http.Error(w, what+" failed", http.StatusInternalServerError)
}
```

Add `"html/template"` to the import block for the `render` signature. The
buffer-first behaviour is the part that matters: a template failure must not
arrive after a 200 has already been committed.

Finally, delete the corresponding stubs from `studio.go`, leaving only
`handleCropView` and `handleCrop` for Task 9.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/studio/... -v`
Expected: PASS, all Task 7 and Task 8 tests.

- [ ] **Step 6: Run the full suite**

Run: `make test && make lint`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/studio/
git commit -m "Add studio label grid, relabelling and capture-now

KnownLabels carries alliance_members and vs_ranking even though nothing
navigates to them: the recognizer must be able to name every screen the
corpus asserts exists, or those frames score wrong on every run forever.

Labels are revalidated at the handler because they arrive from a browser
form, and capture-now degrades to a disabled button when no device is
attached so a phoneless machine can still label."
```

---

## Task 9: Crop view and template cutting

**Files:**
- Modify: `internal/studio/views.go`, `internal/studio/handlers.go`, `internal/studio/handlers_test.go`, `internal/studio/studio.go` (delete the last two stubs)

**Interfaces:**
- Consumes: `vision.Crop(img image.Image, r transport.Rect) image.Image`, `vision.WriteAnchor`, `vision.AnchorSpec` (Task 6); `transport.Rect`.
- Produces: `handleCropView`, `handleCrop`.

**The invariant-#1 story:** the browser reports the drag rectangle as fractions
of the displayed image, never pixels — it divides by the rendered element's
width and height, which is why a scaled-down thumbnail gives the same answer
as a full-size view. The server stores that as a `transport.Rect`. The canvas
is just another denormalization boundary and no absolute coordinate crosses
into the registry.

**Reference height:** the template is cut from a specific frame, so that
frame's own height *is* the reference height for the library. The handler
rejects a frame whose height differs from the server's configured
`RefHeight`, because mixing two capture resolutions in one library silently
mis-scales every match.

- [ ] **Step 1: Write the failing tests**

Append to `internal/studio/handlers_test.go`:

```go
func TestCropViewRendersTheFrameAndAnchorForm(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add("alliance", pngBytes(t, 1080, 2400))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodGet, "/crop?hash="+f.Hash, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/frame/" + f.Hash, `name="anchor_id"`, `name="identifies_screen"`} {
		if !strings.Contains(body, want) {
			t.Errorf("crop page missing %q", want)
		}
	}
}

func TestPostCropWritesTheTemplateAndManifest(t *testing.T) {
	store := corpus.New(t.TempDir())
	manifest := filepath.Join(t.TempDir(), "manifest.yaml")
	srv, err := studio.New(studio.Options{
		Corpus: store, ManifestPath: manifest, RefHeight: 2400, Token: "s3cret",
	})
	if err != nil {
		t.Fatalf("studio.New: %v", err)
	}
	f, _, err := store.Add("alliance", pngBytes(t, 1080, 2400))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{
		"hash": {f.Hash}, "screen": {"alliance"}, "anchor_id": {"alliance_button"},
		"x1": {"0.10"}, "y1": {"0.20"}, "x2": {"0.30"}, "y2": {"0.28"},
		"threshold": {"0.85"}, "identifies_screen": {"on"},
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/crop", form.Encode()))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if reg.ReferenceHeight != 2400 {
		t.Fatalf("ReferenceHeight = %d, want the frame's own height", reg.ReferenceHeight)
	}
	s, ok := reg.Screen("alliance")
	if !ok || len(s.Anchors) != 1 {
		t.Fatalf("registry screens = %+v, want one alliance anchor", reg.Screens)
	}
	a := s.Anchors[0]
	if a.ID != "alliance_button" || !a.IdentifiesScreen {
		t.Fatalf("anchor = %+v, want an identifying alliance_button", a)
	}
	// The template is the cropped region at the frame's native scale.
	if got := a.Template.Bounds().Dx(); got != int(0.20*1080) {
		t.Fatalf("template width = %d, want %d", got, int(0.20*1080))
	}
}

func TestPostCropRejectsAnInvertedRegion(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add("alliance", pngBytes(t, 1080, 2400))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{
		"hash": {f.Hash}, "screen": {"alliance"}, "anchor_id": {"bad"},
		"x1": {"0.9"}, "y1": {"0.9"}, "x2": {"0.1"}, "y2": {"0.1"},
		"threshold": {"0.85"},
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/crop", form.Encode()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// Mixing two capture resolutions in one library silently mis-scales every
// match, and reads as a merely-bad recognizer rather than an obvious break.
func TestPostCropRejectsAFrameAtTheWrongReferenceHeight(t *testing.T) {
	srv, store := newTestServer(t, "s3cret") // RefHeight 2400
	f, _, err := store.Add("alliance", pngBytes(t, 720, 1600))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{
		"hash": {f.Hash}, "screen": {"alliance"}, "anchor_id": {"a"},
		"x1": {"0.1"}, "y1": {"0.1"}, "x2": {"0.3"}, "y2": {"0.2"},
		"threshold": {"0.85"},
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/crop", form.Encode()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestPostCropRejectsAnUnsafeScreenName(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add("alliance", pngBytes(t, 1080, 2400))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{
		"hash": {f.Hash}, "screen": {"../../etc"}, "anchor_id": {"a"},
		"x1": {"0.1"}, "y1": {"0.1"}, "x2": {"0.3"}, "y2": {"0.2"},
		"threshold": {"0.85"},
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/crop", form.Encode()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
```

Add this helper to the same file, and add `"bytes"`, `"image/png"`,
`"path/filepath"` and the `vision` import:

```go
// pngBytes builds a valid PNG of the given size with enough variation that a
// crop of it is a meaningful template.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding %dx%d PNG: %v", w, h, err)
	}
	return buf.Bytes()
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/studio/... -run Crop`
Expected: FAIL — the stubs return 200 with an empty body.

- [ ] **Step 3: Add the crop view**

Append to `internal/studio/views.go`:

```go
var cropTmpl = template.Must(template.New("crop").Parse(layout + `
<h1>crop an anchor</h1>
<p>drag a rectangle over the frame, then name the anchor.</p>
<div style="display:flex;gap:1rem;align-items:flex-start">
 <div style="position:relative;max-width:420px">
  <img id="f" src="/frame/{{.Frame.Hash}}" style="width:100%;display:block;user-select:none">
  <div id="sel" style="position:absolute;border:2px solid #8cf;background:rgba(136,204,255,.2);display:none"></div>
 </div>
 <form method="post" action="/crop" style="min-width:260px">
  <input type="hidden" name="hash" value="{{.Frame.Hash}}">
  <label>screen<select name="screen">{{range $.Labels}}<option value="{{.}}" {{if eq . $.Frame.Label}}selected{{end}}>{{.}}</option>{{end}}</select></label>
  <label>anchor id<input name="anchor_id" required pattern="[a-z0-9_]+"></label>
  <label>threshold<input name="threshold" type="number" step="0.01" min="0" max="1" value="0.85"></label>
  <label><input type="checkbox" name="identifies_screen" checked> identifies this screen</label>
  <input type="hidden" name="x1" id="x1"><input type="hidden" name="y1" id="y1">
  <input type="hidden" name="x2" id="x2"><input type="hidden" name="y2" id="y2">
  <button>cut template</button>
 </form>
</div>
<script>
// The rectangle is reported as fractions of the *displayed* image, never
// pixels, so a scaled-down view gives the same answer as a full-size one.
// That is what keeps invariant #1 true: no absolute coordinate leaves here.
(function () {
  const img = document.getElementById('f'), sel = document.getElementById('sel');
  let sx = 0, sy = 0, dragging = false;
  const frac = e => {
    const r = img.getBoundingClientRect();
    return [
      Math.min(Math.max((e.clientX - r.left) / r.width, 0), 1),
      Math.min(Math.max((e.clientY - r.top) / r.height, 0), 1),
    ];
  };
  img.addEventListener('mousedown', e => {
    e.preventDefault();
    [sx, sy] = frac(e); dragging = true; sel.style.display = 'block';
  });
  window.addEventListener('mousemove', e => {
    if (!dragging) return;
    const [cx, cy] = frac(e);
    const x1 = Math.min(sx, cx), y1 = Math.min(sy, cy);
    const x2 = Math.max(sx, cx), y2 = Math.max(sy, cy);
    sel.style.left = (x1 * 100) + '%'; sel.style.top = (y1 * 100) + '%';
    sel.style.width = ((x2 - x1) * 100) + '%'; sel.style.height = ((y2 - y1) * 100) + '%';
    document.getElementById('x1').value = x1.toFixed(5);
    document.getElementById('y1').value = y1.toFixed(5);
    document.getElementById('x2').value = x2.toFixed(5);
    document.getElementById('y2').value = y2.toFixed(5);
  });
  window.addEventListener('mouseup', () => { dragging = false; });
})();
</script>
`))
```

- [ ] **Step 4: Add the handlers**

Append to `internal/studio/handlers.go`:

```go
func (s *Server) handleCropView(w http.ResponseWriter, r *http.Request) {
	f, err := s.corpus.Find(r.URL.Query().Get("hash"))
	if errors.Is(err, corpus.ErrNotFound) {
		http.Error(w, "no such frame", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "finding the frame to crop", err)
		return
	}
	s.render(w, cropTmpl, map[string]any{
		"Title":      "crop",
		"Frame":      f,
		"Labels":     KnownLabels,
		"CanCapture": s.tr != nil,
	})
}

func (s *Server) handleCrop(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	screen := r.FormValue("screen")
	anchorID := r.FormValue("anchor_id")

	// screen and anchor_id both become path segments under templates/, so
	// they are validated with the same rule that guards corpus labels.
	if err := corpus.CheckLabel(screen); err != nil {
		http.Error(w, "screen: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := corpus.CheckLabel(anchorID); err != nil {
		http.Error(w, "anchor_id: "+err.Error(), http.StatusBadRequest)
		return
	}

	region, err := rectFromForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	threshold, err := strconv.ParseFloat(r.FormValue("threshold"), 64)
	if err != nil {
		http.Error(w, "threshold: "+err.Error(), http.StatusBadRequest)
		return
	}

	data, err := s.corpus.Read(r.FormValue("hash"))
	if errors.Is(err, corpus.ErrNotFound) {
		http.Error(w, "no such frame", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "reading the frame to crop", err)
		return
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		http.Error(w, "frame is not a decodable PNG", http.StatusBadRequest)
		return
	}

	// The template is cut from this frame, so this frame's height is the
	// library's reference height. Two capture resolutions in one library
	// silently mis-scale every match.
	if h := img.Bounds().Dy(); h != s.refHeight {
		http.Error(w, fmt.Sprintf("frame is %dpx tall but the library reference height is %dpx",
			h, s.refHeight), http.StatusBadRequest)
		return
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, vision.Crop(img, region)); err != nil {
		s.fail(w, "encoding the cropped template", err)
		return
	}

	if err := vision.WriteAnchor(s.manifest, s.refHeight, vision.AnchorSpec{
		Screen:           screen,
		ID:               anchorID,
		Region:           region,
		Threshold:        threshold,
		IdentifiesScreen: r.FormValue("identifies_screen") != "",
	}, buf.Bytes()); err != nil {
		// WriteAnchor already rolled back, so a rejected crop leaves the
		// manifest exactly as it was. Report it as the user's problem.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/crop?hash="+r.FormValue("hash"), http.StatusSeeOther)
}

// rectFromForm reads the four normalized bounds the browser posted.
func rectFromForm(r *http.Request) (transport.Rect, error) {
	var vals [4]float64
	for i, name := range []string{"x1", "y1", "x2", "y2"} {
		v, err := strconv.ParseFloat(r.FormValue(name), 64)
		if err != nil {
			return transport.Rect{}, fmt.Errorf("%s: %w", name, err)
		}
		vals[i] = v
	}
	rect := transport.Rect{X1: vals[0], Y1: vals[1], X2: vals[2], Y2: vals[3]}
	if !rect.Valid() {
		return transport.Rect{}, fmt.Errorf("region %+v is not a valid unit-square rect", rect)
	}
	return rect, nil
}
```

Add `"fmt"`, `"strconv"`, `"github.com/tomharris/lw-manager/internal/transport"`
and `"github.com/tomharris/lw-manager/internal/vision"` to the imports. Then
delete the last two stubs from `studio.go`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/studio/... -v`
Expected: PASS, all studio tests.

- [ ] **Step 6: Run the full suite**

Run: `make test && make lint && make verify-nocgo`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/studio/
git commit -m "Add studio crop view and template cutting

The browser reports the drag rectangle as fractions of the displayed image,
so a scaled-down view gives the same answer as a full-size one and no
absolute coordinate ever leaves the page — the canvas is just another
denormalization boundary.

A crop is refused when the frame's height differs from the library's
reference height: two capture resolutions in one library mis-scale every
match and read as a merely-bad recognizer rather than an obvious break."
```

---

## Task 10: `agent studio`

**Files:**
- Create: `cmd/agent/studio.go`
- Modify: `cmd/agent/main.go`

**Interfaces:**
- Consumes: `studio.New`, `studio.Options`, `studio.RequireToken` (Tasks 7–9); `corpus.New`; `transport.NewADBTransport`; `resolveSerial`.
- Produces: `agent studio --addr --token --corpus --templates --ref-height --serial --no-device`

- [ ] **Step 1: Write the command**

Create `cmd/agent/studio.go`:

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/studio"
	"github.com/tomharris/lw-manager/internal/transport"
)

func runStudio(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("studio", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8088", "listen address; a non-loopback bind requires a token")
	token := fs.String("token", "", "shared secret; generated and printed when empty")
	root := fs.String("corpus", "fixtures/corpus", "corpus root directory")
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	refHeight := fs.Int("ref-height", 0, "template library reference height in pixels; defaults to the attached device's height")
	serial := fs.String("serial", "", "device serial for capture-now; optional when exactly one device is attached")
	noDevice := fs.Bool("no-device", false, "run without a device: labelling and cropping only")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *token == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("generating a studio token: %w", err)
		}
		*token = hex.EncodeToString(buf)
	}

	var tr transport.Transport
	if !*noDevice {
		resolved, err := resolveSerial(ctx, cfg.ADBPath, *serial)
		if err != nil {
			return fmt.Errorf("%w (pass --no-device to label without a phone)", err)
		}
		adb, err := transport.NewADBTransport(ctx, transport.ADBOptions{ADBPath: cfg.ADBPath, Serial: resolved})
		if err != nil {
			return fmt.Errorf("opening device %s: %w", resolved, err)
		}
		defer adb.Close()
		tr = adb
		if *refHeight == 0 {
			*refHeight = adb.Resolution().Y
		}
	}
	if *refHeight == 0 {
		return fmt.Errorf("--ref-height is required with --no-device")
	}

	srv, err := studio.New(studio.Options{
		Corpus:       corpus.New(*root),
		Transport:    tr,
		ManifestPath: *manifest,
		RefHeight:    *refHeight,
		Token:        *token,
		Logger:       slog.Default(),
	})
	if err != nil {
		return err
	}

	// The URL goes to stdout so it can be piped or copied; everything else is
	// a log line on stderr.
	scheme := "http://" + *addr
	if studio.RequireToken(*addr) {
		slog.Warn("studio is bound to a non-loopback address", "addr", *addr)
	}
	fmt.Printf("%s/?t=%s\n", scheme, *token)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("studio server on %s: %w", *addr, err)
	}
	return nil
}
```

- [ ] **Step 2: Wire it into `main.go`**

Add to `usage()`:

```
  studio    serve the corpus labelling and cropping UI
```

and to the switch:

```go
	case "studio":
		return runStudio(ctx, cfg, os.Args[2:])
```

- [ ] **Step 3: Verify it builds and starts**

Run:

```bash
make build && make test && make lint
./bin/agent studio --no-device --ref-height 2400 --addr 127.0.0.1:8099 &
sleep 1
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8099/
kill %1
```
Expected: build and tests PASS; the printed URL contains a generated token;
`curl` without the token returns `401`.

- [ ] **Step 4: Commit**

```bash
git add cmd/agent/studio.go cmd/agent/main.go
git commit -m "Add agent studio command

Generates a token when none is given and prints the full URL including it,
so the LAN bind is usable without ever being unauthenticated. --no-device
lets a machine with no phone attached still label and crop, which is the
common case when reviewing a corpus someone else captured."
```

---

## Task 11: Confusion matrix and accuracy (pure)

**Files:**
- Create: `internal/vision/score.go`, `internal/vision/score_test.go`

**Interfaces:**
- Consumes: nothing. This task deliberately touches no images.
- Produces:
  - `const NoneLabel = "_none"` and `const NonePrediction = "<none>"`
  - `type Prediction struct { Hash, Label, Predicted string }`
  - `type Report struct { Total, Correct int; Matrix map[string]map[string]int; Labels, Columns []string }`
  - `func Score(preds []Prediction) Report`
  - `func (r Report) Accuracy() float64`
  - `func (r Report) FormatMatrix() string`

**The point of this split:** computing scores needs images and is slow;
*interpreting* them is arithmetic over tuples. Keeping the interpretation pure
means every degenerate case — an empty corpus, a label that never appears, a
recognizer that predicts nothing at all — gets a fast exhaustive test, and the
slow image work in Task 13 is left with nothing tricky in it.

- [ ] **Step 1: Write the failing tests**

Create `internal/vision/score_test.go`:

```go
package vision_test

import (
	"math"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/vision"
)

func TestScoreCountsAPositiveCorrectOnlyOnAnExactMatch(t *testing.T) {
	r := vision.Score([]vision.Prediction{
		{Hash: "a", Label: "alliance", Predicted: "alliance"},
		{Hash: "b", Label: "alliance", Predicted: "alliance_tech"},
		{Hash: "c", Label: "mail", Predicted: ""},
	})

	if r.Total != 3 {
		t.Fatalf("Total = %d, want 3", r.Total)
	}
	if r.Correct != 1 {
		t.Fatalf("Correct = %d, want 1", r.Correct)
	}
	if math.Abs(r.Accuracy()-1.0/3.0) > 1e-9 {
		t.Fatalf("Accuracy = %v, want 1/3", r.Accuracy())
	}
}

// A negative is correct exactly when recognition failed. This is the rule
// that stops a loose threshold from passing the gate.
func TestScoreCountsANegativeCorrectOnlyWhenNothingWasRecognized(t *testing.T) {
	r := vision.Score([]vision.Prediction{
		{Hash: "a", Label: vision.NoneLabel, Predicted: ""},
		{Hash: "b", Label: vision.NoneLabel, Predicted: "radar"},
	})

	if r.Correct != 1 {
		t.Fatalf("Correct = %d, want 1", r.Correct)
	}
	if got := r.Matrix[vision.NoneLabel]["radar"]; got != 1 {
		t.Fatalf("Matrix[_none][radar] = %d, want 1", got)
	}
	if got := r.Matrix[vision.NoneLabel][vision.NonePrediction]; got != 1 {
		t.Fatalf("Matrix[_none][<none>] = %d, want 1", got)
	}
}

func TestScoreOnAnEmptyCorpusIsZeroAccuracyNotNaN(t *testing.T) {
	r := vision.Score(nil)

	if r.Total != 0 {
		t.Fatalf("Total = %d, want 0", r.Total)
	}
	if r.Accuracy() != 0 {
		t.Fatalf("Accuracy = %v, want 0", r.Accuracy())
	}
}

func TestScoreLabelsAndColumnsAreSortedWithNoneLast(t *testing.T) {
	r := vision.Score([]vision.Prediction{
		{Label: "radar", Predicted: "radar"},
		{Label: "alliance", Predicted: ""},
		{Label: vision.NoneLabel, Predicted: ""},
		{Label: "mail", Predicted: "mail"},
	})

	wantLabels := []string{"alliance", "mail", "radar", vision.NoneLabel}
	if len(r.Labels) != len(wantLabels) {
		t.Fatalf("Labels = %v, want %v", r.Labels, wantLabels)
	}
	for i, l := range wantLabels {
		if r.Labels[i] != l {
			t.Fatalf("Labels = %v, want %v", r.Labels, wantLabels)
		}
	}
	if r.Columns[len(r.Columns)-1] != vision.NonePrediction {
		t.Fatalf("Columns = %v, want %s last", r.Columns, vision.NonePrediction)
	}
}

// "94% accurate" is not actionable. "Eleven alliance_tech frames were called
// alliance" is.
func TestFormatMatrixNamesRowsColumnsAndCounts(t *testing.T) {
	r := vision.Score([]vision.Prediction{
		{Label: "alliance_tech", Predicted: "alliance"},
		{Label: "alliance_tech", Predicted: "alliance"},
		{Label: "alliance_tech", Predicted: "alliance_tech"},
	})

	out := r.FormatMatrix()
	if !strings.Contains(out, "alliance_tech") || !strings.Contains(out, "alliance") {
		t.Fatalf("matrix does not name its rows and columns:\n%s", out)
	}
	if !strings.Contains(out, "2") {
		t.Fatalf("matrix does not show the confusion count:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/vision/... -run 'Score|FormatMatrix'`
Expected: FAIL — `undefined: vision.Score`.

- [ ] **Step 3: Write the implementation**

Create `internal/vision/score.go`:

```go
package vision

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

const (
	// NoneLabel marks a corpus frame the recognizer must refuse to identify.
	NoneLabel = "_none"
	// NonePrediction is the confusion matrix column for "nothing recognized".
	NonePrediction = "<none>"
)

// Prediction is one frame's true label beside what the recognizer called it.
// Predicted is empty when recognition failed.
type Prediction struct {
	Hash      string
	Label     string
	Predicted string
}

// Report summarizes recognizer performance over a labeled corpus.
type Report struct {
	Total   int
	Correct int
	// Matrix is true label → predicted → count, with NonePrediction standing
	// in for a failed recognition.
	Matrix  map[string]map[string]int
	Labels  []string // matrix rows, sorted, NoneLabel last
	Columns []string // matrix columns, sorted, NonePrediction last
}

// Score builds a Report. It is pure: everything expensive and image-shaped
// happens in the caller, so the rules that decide right from wrong get fast
// exhaustive tests.
//
// A positive is correct on an exact match. A negative is correct only when
// recognition failed — that is the rule that stops thresholds so loose every
// frame matches something from passing the gate.
func Score(preds []Prediction) Report {
	r := Report{Matrix: map[string]map[string]int{}}
	cols := map[string]bool{}

	for _, p := range preds {
		r.Total++
		predicted := p.Predicted
		if predicted == "" {
			predicted = NonePrediction
		}
		if r.Matrix[p.Label] == nil {
			r.Matrix[p.Label] = map[string]int{}
		}
		r.Matrix[p.Label][predicted]++
		cols[predicted] = true

		if p.Label == NoneLabel {
			if p.Predicted == "" {
				r.Correct++
			}
			continue
		}
		if p.Predicted == p.Label {
			r.Correct++
		}
	}

	r.Labels = sortedWithNoneLast(keys(r.Matrix), NoneLabel)
	r.Columns = sortedWithNoneLast(keys(cols), NonePrediction)
	return r
}

// Accuracy is the fraction of frames scored correctly. An empty corpus is 0,
// not NaN: a gate comparison against NaN is always false in confusing ways.
func (r Report) Accuracy() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Correct) / float64(r.Total)
}

// FormatMatrix renders the confusion matrix as a text table.
func (r Report) FormatMatrix() string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)

	fmt.Fprint(tw, "true \\ predicted")
	for _, c := range r.Columns {
		fmt.Fprintf(tw, "\t%s", c)
	}
	fmt.Fprintln(tw)

	for _, l := range r.Labels {
		fmt.Fprint(tw, l)
		for _, c := range r.Columns {
			n := r.Matrix[l][c]
			if n == 0 {
				fmt.Fprint(tw, "\t.")
				continue
			}
			fmt.Fprintf(tw, "\t%d", n)
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()
	return sb.String()
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sortedWithNoneLast sorts alphabetically but pins the none bucket to the end,
// where it reads as a summary column rather than an alphabetical accident.
func sortedWithNoneLast(in []string, none string) []string {
	sort.Slice(in, func(i, j int) bool {
		if in[i] == none {
			return false
		}
		if in[j] == none {
			return true
		}
		return in[i] < in[j]
	})
	return in
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/vision/... -run 'Score|FormatMatrix' -v`
Expected: PASS, five tests.

- [ ] **Step 5: Commit**

```bash
git add internal/vision/score.go internal/vision/score_test.go
git commit -m "Add pure corpus scoring and confusion matrix

Computing scores needs images and is slow; interpreting them is arithmetic
over tuples. Keeping the interpretation pure gives every degenerate case a
fast exhaustive test and leaves nothing tricky in the image path.

A negative counts as correct only when recognition failed, which is the rule
that stops thresholds so loose every frame matches something from passing
the gate."
```

---

## Task 12: Per-anchor separation report (pure)

**Files:**
- Create: `internal/vision/separation.go`, `internal/vision/separation_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type AnchorObservation struct { AnchorID, Screen, FrameLabel string; Score float64 }`
  - `type Separation struct { AnchorID, Screen string; InCount, OutCount int; WorstIn, BestOut, Gap, Suggested float64; Overlap bool }`
  - `func Separations(obs []AnchorObservation) []Separation`
  - `func FormatSeparations(seps []Separation) string`

**Do not name any type `anchorScore`.** That identifier is already taken by the
private struct in `recognizer.go`, and reusing it would shadow the recognition
decision's own type.

**Why this exists.** Threshold tuning has a failure mode where numbers get
nudged until the aggregate passes, while one anchor is quietly
non-discriminative and has been papered over by a loose threshold elsewhere.
Reporting the gap between the worst in-screen score and the best out-of-screen
score makes that structurally visible, and the two cases need different
actions: `Gap > 0` means retune, `Gap <= 0` means recrop. It also interacts
with `scoreScreen`'s min-aggregation — a screen scores as its weakest
identifying anchor, so one bad anchor caps its whole screen. Per-anchor
reporting finds that; per-screen reporting hides it.

- [ ] **Step 1: Write the failing tests**

Create `internal/vision/separation_test.go`:

```go
package vision_test

import (
	"math"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/vision"
)

func TestSeparationsSuggestsTheMidpointOfASeparableGap(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "alliance", Score: 0.94},
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "alliance", Score: 0.88},
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "mail", Score: 0.42},
		{AnchorID: "alliance_button", Screen: "alliance", FrameLabel: "_none", Score: 0.51},
	})

	if len(seps) != 1 {
		t.Fatalf("Separations = %+v, want one anchor", seps)
	}
	s := seps[0]
	if s.InCount != 2 || s.OutCount != 2 {
		t.Fatalf("InCount/OutCount = %d/%d, want 2/2", s.InCount, s.OutCount)
	}
	if math.Abs(s.WorstIn-0.88) > 1e-9 || math.Abs(s.BestOut-0.51) > 1e-9 {
		t.Fatalf("WorstIn/BestOut = %v/%v, want 0.88/0.51", s.WorstIn, s.BestOut)
	}
	if math.Abs(s.Gap-0.37) > 1e-9 {
		t.Fatalf("Gap = %v, want 0.37", s.Gap)
	}
	if math.Abs(s.Suggested-0.695) > 1e-9 {
		t.Fatalf("Suggested = %v, want 0.695", s.Suggested)
	}
	if s.Overlap {
		t.Fatal("a separable anchor was flagged as overlapping")
	}
}

// The case the whole report exists for: no threshold can work, so retuning is
// the wrong action and recropping is the right one.
func TestSeparationsFlagsOverlapWhenNoThresholdCanSeparate(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "mail", Score: 0.71},
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "base", Score: 0.77},
	})

	s := seps[0]
	if !s.Overlap {
		t.Fatalf("Overlap = false for gap %v, want true", s.Gap)
	}
	if s.Suggested != 0 {
		t.Fatalf("Suggested = %v, want 0 for an unseparable anchor", s.Suggested)
	}
}

// An anchor never seen on its own screen cannot be thresholded either. It is
// a corpus gap, and must not be reported as a healthy wide margin.
func TestSeparationsTreatsNoInScreenObservationsAsOverlap(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "vs_button", Screen: "vs_ranking", FrameLabel: "base", Score: 0.2},
	})

	s := seps[0]
	if s.InCount != 0 {
		t.Fatalf("InCount = %d, want 0", s.InCount)
	}
	if !s.Overlap {
		t.Fatal("an anchor with no in-screen observations was not flagged")
	}
}

// NCC scores floor at 0, so an anchor never seen off its screen gets a
// well-defined margin rather than an infinite one.
func TestSeparationsWithNoOutOfScreenObservationsUsesZeroAsTheFloor(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "radar_button", Screen: "radar", FrameLabel: "radar", Score: 0.9},
	})

	s := seps[0]
	if s.BestOut != 0 {
		t.Fatalf("BestOut = %v, want 0", s.BestOut)
	}
	if math.Abs(s.Suggested-0.45) > 1e-9 {
		t.Fatalf("Suggested = %v, want 0.45", s.Suggested)
	}
	if s.Overlap {
		t.Fatal("a never-confused anchor was flagged as overlapping")
	}
}

func TestSeparationsIsSortedByAnchorID(t *testing.T) {
	seps := vision.Separations([]vision.AnchorObservation{
		{AnchorID: "z", Screen: "radar", FrameLabel: "radar", Score: 0.9},
		{AnchorID: "a", Screen: "mail", FrameLabel: "mail", Score: 0.9},
	})

	if len(seps) != 2 || seps[0].AnchorID != "a" || seps[1].AnchorID != "z" {
		t.Fatalf("Separations = %+v, want sorted by AnchorID", seps)
	}
}

func TestFormatSeparationsMarksTheAnchorsThatNeedRecropping(t *testing.T) {
	out := vision.FormatSeparations(vision.Separations([]vision.AnchorObservation{
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "mail", Score: 0.71},
		{AnchorID: "mail_button", Screen: "mail", FrameLabel: "base", Score: 0.77},
	}))

	if !strings.Contains(out, "mail_button") {
		t.Fatalf("report does not name the anchor:\n%s", out)
	}
	if !strings.Contains(strings.ToUpper(out), "RECROP") {
		t.Fatalf("report does not say what action an overlap needs:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/vision/... -run Separation`
Expected: FAIL — `undefined: vision.Separations`.

- [ ] **Step 3: Write the implementation**

Create `internal/vision/separation.go`:

```go
package vision

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

// AnchorObservation is one anchor's NCC score against one corpus frame.
//
// Deliberately not named anchorScore: that identifier already belongs to the
// recognition decision's own type in recognizer.go.
type AnchorObservation struct {
	AnchorID   string
	Screen     string // the screen the anchor belongs to
	FrameLabel string // the frame's true label
	Score      float64
}

// Separation is one anchor's discriminative margin over the corpus.
type Separation struct {
	AnchorID  string
	Screen    string
	InCount   int
	OutCount  int
	WorstIn   float64
	BestOut   float64
	Gap       float64 // WorstIn - BestOut
	Suggested float64 // midpoint of the gap; 0 when Overlap
	Overlap   bool    // no threshold can separate: recrop, do not retune
}

// Separations computes one Separation per anchor.
//
// The gap between the worst in-screen score and the best out-of-screen score
// is the anchor's discriminative margin. A positive gap means a threshold
// exists and the midpoint is the safest choice. A non-positive gap means no
// threshold can work — which calls for a different action entirely, so it is
// flagged rather than papered over with a suggested number.
//
// An anchor with no in-screen observations is treated as overlapping. It is a
// corpus gap, and reporting it as a healthy wide margin would be worse than
// useless.
func Separations(obs []AnchorObservation) []Separation {
	type acc struct {
		screen   string
		worstIn  float64
		bestOut  float64
		inCount  int
		outCount int
	}
	byAnchor := map[string]*acc{}

	for _, o := range obs {
		a := byAnchor[o.AnchorID]
		if a == nil {
			// NCC scores floor at 0, so an anchor never seen off its own
			// screen gets a well-defined margin instead of an infinite one.
			a = &acc{screen: o.Screen, worstIn: 1, bestOut: 0}
			byAnchor[o.AnchorID] = a
		}
		if o.FrameLabel == o.Screen {
			a.inCount++
			if o.Score < a.worstIn {
				a.worstIn = o.Score
			}
			continue
		}
		a.outCount++
		if o.Score > a.bestOut {
			a.bestOut = o.Score
		}
	}

	out := make([]Separation, 0, len(byAnchor))
	for id, a := range byAnchor {
		s := Separation{
			AnchorID: id,
			Screen:   a.screen,
			InCount:  a.inCount,
			OutCount: a.outCount,
			BestOut:  a.bestOut,
		}
		if a.inCount == 0 {
			s.Overlap = true
			out = append(out, s)
			continue
		}
		s.WorstIn = a.worstIn
		s.Gap = a.worstIn - a.bestOut
		if s.Gap <= 0 {
			s.Overlap = true
		} else {
			s.Suggested = (a.worstIn + a.bestOut) / 2
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AnchorID < out[j].AnchorID })
	return out
}

// FormatSeparations renders the separation report, naming the action each
// anchor needs rather than only its numbers.
func FormatSeparations(seps []Separation) string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "anchor\tscreen\tin\tout\tworst-in\tbest-out\tgap\tsuggested\taction")
	for _, s := range seps {
		action := "ok"
		suggested := fmt.Sprintf("%.3f", s.Suggested)
		switch {
		case s.InCount == 0:
			action, suggested = "RECROP (never seen on its own screen)", "-"
		case s.Overlap:
			action, suggested = "RECROP (distributions overlap)", "-"
		case s.Gap < 0.05:
			action = "narrow margin"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.3f\t%.3f\t%+.3f\t%s\t%s\n",
			s.AnchorID, s.Screen, s.InCount, s.OutCount,
			s.WorstIn, s.BestOut, s.Gap, suggested, action)
	}
	_ = tw.Flush()
	return sb.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/vision/... -run Separation -v`
Expected: PASS, six tests.

- [ ] **Step 5: Run the full suite**

Run: `make test && make lint`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/vision/separation.go internal/vision/separation_test.go
git commit -m "Add per-anchor separation report

Tuning has a failure mode where numbers get nudged until the aggregate
passes while one anchor is quietly non-discriminative. Reporting the gap
between the worst in-screen and best out-of-screen score makes that visible,
and the two cases need different actions: a positive gap means retune, a
non-positive gap means recrop.

Matters especially because scoreScreen aggregates by min, so one bad anchor
caps its whole screen — per-anchor reporting finds that, per-screen hides it."
```

---

## Task 13: Evaluate and `agent score`

**Files:**
- Create: `internal/vision/evaluate.go`, `internal/vision/evaluate_test.go`, `cmd/agent/score.go`
- Modify: `internal/vision/matcher.go` (export `Resize`), `cmd/agent/main.go`

**Interfaces:**
- Consumes: `Registry`, `Recognizer`, `Match`, `ErrNoScreenRecognized`, `Prediction`, `AnchorObservation`, `Grayscale`, private `resizeGray`.
- Produces:
  - `func Resize(img image.Image, w, h int) *image.Gray`
  - `type Frame struct { Hash, Label string; Image image.Image }`
  - `func Evaluate(reg *Registry, frames []Frame) ([]Prediction, []AnchorObservation, error)`
  - `func Rescale(img image.Image, factor float64) image.Image`
  - `agent score --corpus --templates --gate --rescale --json --apply-thresholds`

`vision.Frame` and `corpus.Frame` are different types with the same name in
different packages: `corpus.Frame` is a file on disk, `vision.Frame` is a
decoded image with a label. `cmd/agent/score.go` converts between them and is
the only place both are in scope.

- [ ] **Step 1: Write the failing test**

Create `internal/vision/evaluate_test.go`:

```go
package vision_test

import (
	"path/filepath"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// buildRegistry writes a one-anchor manifest and loads it, so Evaluate is
// tested against a real Registry rather than a hand-built one.
func buildRegistry(t *testing.T, screen, anchorID string, region transport.Rect, threshold float64) *vision.Registry {
	t.Helper()
	manifest := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := vision.WriteAnchor(manifest, 40, vision.AnchorSpec{
		Screen: screen, ID: anchorID, Region: region,
		Threshold: threshold, IdentifiesScreen: true,
	}, tinyPNG(t)); err != nil {
		t.Fatalf("WriteAnchor: %v", err)
	}
	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	return reg
}

func TestEvaluateProducesOnePredictionPerFrame(t *testing.T) {
	reg := buildRegistry(t, "base", "base_button",
		transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 0.9)

	frames := []vision.Frame{
		{Hash: "a", Label: "base", Image: decodePNG(t, tinyFrame(t, 40, 40))},
		{Hash: "b", Label: vision.NoneLabel, Image: decodePNG(t, tinyFrame(t, 40, 40))},
	}

	preds, obs, err := vision.Evaluate(reg, frames)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(preds) != 2 {
		t.Fatalf("predictions = %d, want 2", len(preds))
	}
	if preds[0].Hash != "a" || preds[0].Label != "base" {
		t.Fatalf("prediction[0] = %+v, want hash a label base", preds[0])
	}
	// One anchor scored against both frames.
	if len(obs) != 2 {
		t.Fatalf("observations = %d, want 2", len(obs))
	}
	for _, o := range obs {
		if o.AnchorID != "base_button" || o.Screen != "base" {
			t.Fatalf("observation = %+v, want the base_button anchor", o)
		}
	}
}

func TestEvaluateReportsAnUnrecognizedFrameAsAnEmptyPrediction(t *testing.T) {
	// A threshold of 1.0 is unreachable, so nothing can be recognized.
	reg := buildRegistry(t, "base", "base_button",
		transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}, 1.0)

	preds, _, err := vision.Evaluate(reg, []vision.Frame{
		{Hash: "a", Label: "base", Image: decodePNG(t, tinyFrame(t, 40, 40))},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if preds[0].Predicted != "" {
		t.Fatalf("Predicted = %q, want empty for an unrecognized frame", preds[0].Predicted)
	}
}

func TestRescaleChangesTheFrameHeight(t *testing.T) {
	src := decodePNG(t, tinyFrame(t, 40, 80))

	got := vision.Rescale(src, 0.5)

	if h := got.Bounds().Dy(); h != 40 {
		t.Fatalf("height = %d, want 40", h)
	}
	if w := got.Bounds().Dx(); w != 20 {
		t.Fatalf("width = %d, want 20", w)
	}
}
```

Add these two helpers to the same file, along with the `"bytes"`, `"image"`,
`"image/color"` and `"image/png"` imports they need (`tinyPNG` already exists
in `manifest_write_test.go`, which is the same `vision_test` package):

```go
// tinyFrame builds a PNG with enough variation that NCC is well-defined.
func tinyFrame(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8((x*7 + y*13) % 256)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding frame: %v", err)
	}
	return buf.Bytes()
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding PNG: %v", err)
	}
	return img
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/vision/... -run 'Evaluate|Rescale'`
Expected: FAIL — `undefined: vision.Evaluate`.

- [ ] **Step 3: Export `Resize`**

Append to `internal/vision/matcher.go`:

```go
// Resize scales an image to exact dimensions, in grayscale.
//
// Exported so the scoring harness can synthesize a second resolution from a
// one-handset corpus. That measures the matcher's scale handling; it is not
// evidence of cross-device generalization, because a real second device
// differs in DPI and therefore in layout and font hinting, not only in scale.
func Resize(img image.Image, w, h int) *image.Gray {
	return resizeGray(Grayscale(img), w, h)
}
```

- [ ] **Step 4: Write `Evaluate`**

Create `internal/vision/evaluate.go`:

```go
package vision

import (
	"errors"
	"fmt"
	"image"
	"math"
)

// Frame is one decoded corpus image with its true label.
type Frame struct {
	Hash  string
	Label string
	Image image.Image
}

// Evaluate runs the recognizer over every frame and, separately, scores every
// anchor against every frame.
//
// The two outputs answer different questions. Predictions say whether the
// recognizer is right; observations say which anchor to fix when it is not.
// Both are plain tuples, so the interpretation in Score and Separations stays
// pure and this function stays the only slow, image-shaped part.
func Evaluate(reg *Registry, frames []Frame) ([]Prediction, []AnchorObservation, error) {
	rec := NewRecognizer(reg)
	preds := make([]Prediction, 0, len(frames))
	var obs []AnchorObservation

	for _, f := range frames {
		screen, _, err := rec.Recognize(f.Image)
		switch {
		case errors.Is(err, ErrNoScreenRecognized):
			screen = ""
		case err != nil:
			return nil, nil, fmt.Errorf("vision: recognizing frame %s (label %q): %w", f.Hash, f.Label, err)
		}
		preds = append(preds, Prediction{Hash: f.Hash, Label: f.Label, Predicted: screen})

		for _, s := range reg.Screens {
			for _, a := range s.Anchors {
				if !a.IdentifiesScreen {
					continue
				}
				m, err := Match(f.Image, a.Template, a.Region, reg.ReferenceHeight)
				if err != nil {
					return nil, nil, fmt.Errorf("vision: matching %s/%s against frame %s: %w",
						s.Name, a.ID, f.Hash, err)
				}
				obs = append(obs, AnchorObservation{
					AnchorID:   a.ID,
					Screen:     s.Name,
					FrameLabel: f.Label,
					Score:      m.Score,
				})
			}
		}
	}
	return preds, obs, nil
}

// Rescale returns img scaled by factor, in grayscale.
func Rescale(img image.Image, factor float64) image.Image {
	b := img.Bounds()
	w := int(math.Round(float64(b.Dx()) * factor))
	h := int(math.Round(float64(b.Dy()) * factor))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return Resize(img, w, h)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/vision/... -v`
Expected: PASS, every vision test.

- [ ] **Step 6: Write `agent score`**

Create `cmd/agent/score.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
	"os"
	"strconv"
	"strings"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/vision"
)

func runScore(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	root := fs.String("corpus", "fixtures/corpus", "corpus root directory")
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	gate := fs.Float64("gate", 0.98, "minimum accuracy; a lower score exits non-zero")
	rescale := fs.String("rescale", "", "comma-separated scale factors to also score, e.g. 0.75,1.25")
	asJSON := fs.Bool("json", false, "emit machine-readable output")
	apply := fs.Bool("apply-thresholds", false, "write suggested thresholds back to the manifest")
	if err := fs.Parse(args); err != nil {
		return err
	}

	reg, err := vision.LoadRegistry(*manifest)
	if err != nil {
		return err
	}
	frames, err := loadCorpusFrames(corpus.New(*root))
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("corpus %s is empty; run `agent corpus pull` first", *root)
	}

	preds, obs, err := vision.Evaluate(reg, frames)
	if err != nil {
		return err
	}
	report := vision.Score(preds)
	seps := vision.Separations(obs)

	type scaled struct {
		Factor   float64 `json:"factor"`
		Accuracy float64 `json:"accuracy"`
	}
	var rescaled []scaled
	for _, f := range parseFactors(*rescale) {
		scaledFrames := make([]vision.Frame, len(frames))
		for i, fr := range frames {
			scaledFrames[i] = vision.Frame{Hash: fr.Hash, Label: fr.Label, Image: vision.Rescale(fr.Image, f)}
		}
		p, _, err := vision.Evaluate(reg, scaledFrames)
		if err != nil {
			return fmt.Errorf("scoring at scale %.2f: %w", f, err)
		}
		rescaled = append(rescaled, scaled{Factor: f, Accuracy: vision.Score(p).Accuracy()})
	}

	if *apply {
		thresholds := map[string]float64{}
		for _, s := range seps {
			if s.Overlap {
				continue // no threshold can fix this one; recrop it
			}
			thresholds[s.Screen+"/"+s.AnchorID] = s.Suggested
		}
		if err := vision.SetThresholds(*manifest, thresholds); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "applied %d suggested thresholds to %s\n", len(thresholds), *manifest)
	}

	if *asJSON {
		out := map[string]any{
			"total": report.Total, "correct": report.Correct,
			"accuracy": report.Accuracy(), "gate": *gate,
			"passed": report.Accuracy() >= *gate,
			"matrix": report.Matrix, "separations": seps,
			"rescaled": rescaled,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		fmt.Printf("accuracy %.4f (%d/%d) gate %.4f\n\n",
			report.Accuracy(), report.Correct, report.Total, *gate)
		fmt.Println(report.FormatMatrix())
		fmt.Println(vision.FormatSeparations(seps))
		for _, s := range rescaled {
			fmt.Printf("rescaled x%.2f: accuracy %.4f\n", s.Factor, s.Accuracy)
		}
		if len(rescaled) > 0 {
			// Say what this does and does not show, in the report itself.
			// A limitation stated only in a design doc is a limitation
			// nobody rereads.
			fmt.Println("\nrescaled figures test the matcher's scale handling only. A real second\n" +
				"device differs in DPI, so its layout and font hinting differ too; this is\n" +
				"not evidence of cross-device generalization.")
		}
	}

	if report.Accuracy() < *gate {
		return fmt.Errorf("accuracy %.4f is below the gate of %.4f", report.Accuracy(), *gate)
	}
	return nil
}

// loadCorpusFrames decodes every labeled frame. Unsorted frames are skipped:
// they carry no ground truth, so scoring them would be meaningless.
func loadCorpusFrames(store *corpus.Store) ([]vision.Frame, error) {
	all, err := store.All()
	if err != nil {
		return nil, err
	}
	out := make([]vision.Frame, 0, len(all))
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
		out = append(out, vision.Frame{Hash: f.Hash, Label: f.Label, Image: img})
	}
	return out, nil
}

func parseFactors(s string) []float64 {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if f, err := strconv.ParseFloat(part, 64); err == nil && f > 0 {
			out = append(out, f)
		}
	}
	return out
}
```

- [ ] **Step 7: Wire it into `main.go`**

Add to `usage()`:

```
  score     score the recognizer against the labeled corpus (the M1 gate)
```

and to the switch:

```go
	case "score":
		return runScore(ctx, cfg, os.Args[2:])
```

- [ ] **Step 8: Verify**

Run: `make build && make test && make lint && make verify-nocgo`
Expected: PASS. Then:

```bash
./bin/agent score --corpus /tmp/empty-corpus 2>&1 | head -3
```
Expected: fails with a message naming the empty corpus and suggesting
`agent corpus pull`, not a panic or a 0.0000 accuracy that looks like a
recognizer problem.

- [ ] **Step 9: Commit**

```bash
git add internal/vision/evaluate.go internal/vision/evaluate_test.go \
        internal/vision/matcher.go cmd/agent/score.go cmd/agent/main.go
git commit -m "Add Evaluate and the agent score gate harness

Evaluate is the only slow image-shaped step; it emits plain tuples so Score
and Separations stay pure. Suggested thresholds are printed and only written
under an explicit --apply-thresholds: a gate that silently rewrites the
manifest to make itself pass is not a gate.

Overlapping anchors are excluded from --apply-thresholds, because no
threshold can fix them and writing one would hide the anchor that actually
needs recropping. The rescale limitation is printed in the report itself,
since a caveat only in a design doc is one nobody rereads."
```

---

## Task 14: The gate test, `make gate`, and documentation

**Files:**
- Create: `internal/vision/corpus_test.go`
- Modify: `Makefile`, `CLAUDE.md`

**Interfaces:**
- Consumes: `vision.Evaluate`, `vision.Score`, `vision.LoadRegistry`, `corpus.Store`.
- Produces: `make gate`.

**Why a build tag:** multi-scale NCC over 200+ frames is slow, and `make test`
must stay fast enough to run constantly. Same reasoning that separates the
`integration` and `device` tags. It skips rather than fails when the corpus
has not been pulled, exactly as `test-device` skips with no device attached.

- [ ] **Step 1: Write the gate test**

Create `internal/vision/corpus_test.go`:

```go
//go:build corpus

// The M1 gate: the screen recognizer scores at least 98% on the real corpus,
// offline, with no device attached.
//
// Behind a build tag because multi-scale NCC over 200+ frames is slow and
// `make test` must stay fast. Run it with `make gate`.
package vision_test

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/vision"
)

// gateAccuracy is the M1 phase gate from the platform design doc.
const gateAccuracy = 0.98

func TestM1RecognizerGate(t *testing.T) {
	corpusRoot := envOr("LW_CORPUS_ROOT", filepath.Join("..", "..", "fixtures", "corpus"))
	manifestPath := envOr("LW_TEMPLATES", filepath.Join("..", "..", "templates", "manifest.yaml"))

	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		t.Skipf("no template manifest at %s; crop anchors in `agent studio` first", manifestPath)
	}
	store := corpus.New(corpusRoot)
	files, err := store.All()
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	if len(files) == 0 {
		t.Skipf("no corpus at %s; run `agent corpus pull` first", corpusRoot)
	}

	reg, err := vision.LoadRegistry(manifestPath)
	if err != nil {
		t.Fatalf("loading registry: %v", err)
	}

	var frames []vision.Frame
	for _, f := range files {
		if f.Label == corpus.Unsorted {
			continue // no ground truth, nothing to score against
		}
		data, err := store.Read(f.Hash)
		if err != nil {
			t.Fatalf("reading %s: %v", f.Hash, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decoding %s (label %q): %v", f.Hash, f.Label, err)
		}
		frames = append(frames, vision.Frame{Hash: f.Hash, Label: f.Label, Image: img})
	}
	if len(frames) == 0 {
		t.Skip("corpus has no labeled frames yet; label them in `agent studio`")
	}

	preds, obs, err := vision.Evaluate(reg, frames)
	if err != nil {
		t.Fatalf("evaluating: %v", err)
	}
	report := vision.Score(preds)

	// A failure prints the diagnostics, not just the number. "94%" is not
	// actionable; the matrix and the separation report say what to fix.
	if report.Accuracy() < gateAccuracy {
		t.Errorf("M1 gate: accuracy %.4f (%d/%d) is below %.2f\n\n%s\n%s",
			report.Accuracy(), report.Correct, report.Total, gateAccuracy,
			report.FormatMatrix(), vision.FormatSeparations(vision.Separations(obs)))
	}

	// A corpus too uniform to be meaningful would pass the accuracy check
	// while proving nothing, so assert the shape of the corpus too.
	if report.Total < 200 {
		t.Errorf("corpus has %d labeled frames, want at least 200 (design doc line 376)", report.Total)
	}
	if n := len(report.Matrix[vision.NoneLabel]); n == 0 {
		t.Error("corpus has no negatives; without them a loose threshold passes this gate")
	}
	for _, label := range []string{
		"base", "world_map", "alliance", "alliance_tech",
		"alliance_members", "vs_ranking", "mail", "radar",
	} {
		if total := rowTotal(report.Matrix[label]); total < 10 {
			t.Errorf("label %q has only %d frames; too few to say anything", label, total)
		}
	}
}

func rowTotal(row map[string]int) int {
	n := 0
	for _, v := range row {
		n += v
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Verify the tag keeps it out of the default suite**

Run: `make test`
Expected: PASS, and `go test ./internal/vision/... -v 2>&1 | grep -c M1Recognizer`
returns `0` — the gate must not run untagged.

- [ ] **Step 3: Verify it skips cleanly with no corpus**

Run: `go test -tags=corpus -count=1 -v ./internal/vision/... -run M1RecognizerGate`
Expected: `SKIP` with the message pointing at `agent corpus pull` or
`agent studio`. Not a failure, and not a panic.

- [ ] **Step 4: Add the `gate` target**

In `Makefile`, after the `test-device` target:

```makefile
# The M1 phase gate: recognizer accuracy against the real corpus. Device-free
# but slow, so it is tagged out of `make test`. Skips when the corpus has not
# been pulled.
.PHONY: gate
gate:
	$(GO) test -tags=corpus -count=1 -v ./internal/vision/...
```

- [ ] **Step 5: Document it in CLAUDE.md**

In the **Quickstart** block, after the `agent accounts` line:

```bash
./bin/agent record --interval 2s --duration 10m   # burst-capture the corpus
./bin/agent studio --addr 0.0.0.0:8088            # label and crop, from a browser
./bin/agent corpus index && ./bin/agent corpus push
./bin/agent score                                 # the M1 gate, with diagnostics
make gate                                         # the same gate, as a test
```

In **Testing**, after the `test-device` bullet:

```markdown
- `make gate` — the M1 phase gate: recognizer accuracy against the real
  corpus. Tagged `//go:build corpus`, device-free but slow, so it stays out
  of `make test`. Skips when the corpus has not been pulled.
```

Add a new section after "ReplayTransport exhaustion":

```markdown
### The fixture corpus lives in the blob store, not in git

200+ full-resolution screenshots is 300–600 MB, which git would keep in
history forever. So `fixtures/corpus/<label>/<sha256>.png` is gitignored and
the bytes live in the content-addressed blob store; only
`fixtures/corpus/index.yaml` is committed. `agent corpus pull` materializes
them, `push` uploads new ones, `index` regenerates the projection.

**The label is the directory.** There is no sidecar metadata to fall out of
sync, so a mislabel is fixed with `mv` and the corpus is inspectable without
any of our code. `index.yaml` carries only what a PNG cannot: capture time,
device model, and **game version** — which is what later explains a gate that
used to pass and now does not.

Two properties are load-bearing:

- **A duplicate frame is dropped**, which is the opposite of the
  `screenshots`-table rule above. There, identical bytes still earn a row
  because each capture is a distinct observation. Here a duplicate is noise
  that would weight the accuracy denominator toward whichever screen the
  phone happened to idle on.
- **Negatives are part of the gate.** `_none` frames are correct only when
  recognition *fails*. Without them the gate is passable with thresholds so
  loose that every frame matches something, and acting on a misidentified
  screen is exactly the blind tap invariant #3 forbids.

`agent score` prints a confusion matrix and a per-anchor separation report.
The separation report is the actionable one: a positive gap between an
anchor's worst in-screen and best out-of-screen score means retune, and a
non-positive gap means **recrop** — no threshold can separate them. That
distinction matters because `scoreScreen` aggregates by min, so one bad
anchor caps its entire screen.

The recognizer needs an identifying anchor for **every** labeled screen, not
just the six `DefaultGraph()` navigates. `alliance_members` and `vs_ranking`
are in the corpus for M4; without anchors they would be wrong on every
scoring run forever. Recognition and navigation are separate concerns.
```

- [ ] **Step 6: Verify and commit**

Run: `make test && make lint && make gate`
Expected: `make test` and `make lint` PASS; `make gate` SKIPs (no corpus yet).

```bash
git add internal/vision/corpus_test.go Makefile CLAUDE.md
git commit -m "Add the M1 gate test, make gate, and corpus docs

Tagged out of make test because multi-scale NCC over 200+ frames is slow,
and it skips rather than fails when the corpus has not been pulled.

It asserts the shape of the corpus as well as the accuracy: at least 200
labeled frames, at least ten per screen, and a non-empty negatives set. A
corpus too uniform to be meaningful would clear 98% while proving nothing."
```

---

## Task 15: Bring up the phone and build the corpus

This is the human-time task the tooling exists for. It is last in the plan but
**step 1 can start as soon as Task 5 lands** — capture is the long pole, and
Tasks 6–14 do not depend on it.

**Files:** none. This task produces `fixtures/corpus/index.yaml`,
`templates/manifest.yaml`, and the anchor PNGs.

- [ ] **Step 1: Get the phone onto adb**

On the handset: Settings → About phone → tap Build number seven times, then
Developer options → USB debugging. Plug into the headless box and accept the
RSA fingerprint prompt on the phone's screen.

Run: `adb devices -l`
Expected: the serial listed as `device`, not `unauthorized` or `offline`.

- [ ] **Step 2: Prove the M0 path on real hardware**

This is the first exercise of `ADBTransport` against a handset — everything
until now has been the emulator or `ReplayTransport`.

```bash
docker compose up -d
./bin/control migrate
./bin/agent devices
./bin/agent register --nickname <alt-nickname> --role alliance_data
./bin/agent capture --account <id printed above>
```
Expected: `devices` reports the real resolution; `capture` prints a
`screenshot_id` and a `sha256`. If `screencap` returns corrupt PNG bytes,
check that `exec-out` is being used rather than `shell` — CRLF translation
corrupts binary output while still exiting 0.

- [ ] **Step 3: Capture the corpus**

Roughly 25 frames per screen across the eight screens, plus about 40
negatives — around 240 frames. Run `record` in one terminal, then drive the
phone by hand.

```bash
./bin/agent record --interval 2s --duration 10m
```

Vary deliberately across sessions: different times of day, with and without
notification badges, mail unread and mail empty, alliance tech at different
donation states, with and without the alliance help banner. For negatives,
wander into ad overlays, loading transitions, event popups and shop screens.

**A corpus of 200 near-identical frames proves nothing that 8 frames would
not.** It would still read ≥ 98% and still be worthless.

- [ ] **Step 4: Label**

```bash
./bin/agent studio --addr 0.0.0.0:8088
```

Open the printed URL (it includes the token) from your laptop. Label
everything out of `_unsorted/`, sending anything that is not one of the eight
screens to `_none`.

- [ ] **Step 5: Cut the anchors**

Still in the studio, use the crop view. Cut at minimum the six anchors
`DefaultGraph()` names — `world_map_button`, `base_button`, `alliance_button`,
`tech_button`, `mail_button`, `radar_button` — **plus identifying anchors for
`alliance_members` and `vs_ranking`**, which the graph does not navigate to
but the recognizer must be able to name.

Prefer stable chrome over anything that animates, shows a count, or reflects
state. A badge that appears only when mail is unread is a bad anchor, and the
separation report will say so.

- [ ] **Step 6: Score, fix, repeat**

```bash
./bin/agent corpus index && ./bin/agent corpus push
./bin/agent score
```

Read the separation report before touching a threshold:

- `RECROP (distributions overlap)` → the anchor cannot work. Cut a different
  region. Do not retune.
- `RECROP (never seen on its own screen)` → a corpus gap, not an anchor
  problem. Capture frames of that screen.
- `narrow margin` → it works today and is one game update from breaking.
  Prefer a better anchor if one exists.
- `ok` → adopt the suggested threshold.

When every anchor separates cleanly:

```bash
./bin/agent score --apply-thresholds
./bin/agent score --rescale 0.75,1.25
```

- [ ] **Step 7: Pass the gate**

Run: `make gate`
Expected: PASS — accuracy ≥ 0.98, at least 200 labeled frames, at least ten
per screen, and a non-empty negatives set.

Then confirm the templates unblock the task runtime, which has been refusing
to run since M2:

```bash
./bin/agent run-task --account <id> --task help_all
```
Expected: `DefaultGraph().Validate()` now passes, so the task actually runs
rather than failing on missing anchors.

- [ ] **Step 8: Commit the index and the templates**

```bash
git add fixtures/corpus/index.yaml templates/
git commit -m "Add the real screenshot corpus index and anchor templates

240 labeled frames across eight screens plus negatives, captured on the
handset. Recognizer scores <ACTUAL>% against them, clearing the M1 gate.

Corpus bytes are in the blob store; only the index is committed. Templates
cover the six DefaultGraph anchors plus identifying anchors for
alliance_members and vs_ranking, which the graph does not navigate to but
the recognizer must be able to name."
```

Replace `<ACTUAL>` with the real figure from `agent score`. Do not write a
number you have not seen printed.

---

## Done when

- `make test` passes with nothing running — no emulator, no adb, no Docker.
- `make lint` and `make verify-nocgo` pass.
- `make gate` passes: ≥ 98% accuracy over ≥ 200 labeled frames with negatives.
- `agent run-task --task help_all` no longer fails graph validation.
- `fixtures/corpus/index.yaml` and `templates/` are committed; corpus PNGs are
  not.

## Out of scope

Real Tier 1 task bodies (gather-gold-only, Quick Execute, Claim All), the M2
24h unattended acceptance run, M4 parsing and capture routes, graph edges to
`alliance_members` and `vs_ranking`, live threshold sliders, studio auth
beyond the shared token, and multi-device orchestration.
