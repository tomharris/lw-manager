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
