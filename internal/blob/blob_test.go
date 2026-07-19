package blob

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore(): %v", err)
	}
	return s
}

func TestKeyFansOut(t *testing.T) {
	sum := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	got := Key(sum)
	want := "sha256/e3/b0/" + sum
	if got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	data := []byte("fake png bytes")

	key, sum, err := PutContent(ctx, s, data)
	if err != nil {
		t.Fatalf("PutContent(): %v", err)
	}
	if sum != Sum(data) {
		t.Errorf("returned digest %q does not match Sum() %q", sum, Sum(data))
	}

	got, err := GetContent(ctx, s, key, sum)
	if err != nil {
		t.Fatalf("GetContent(): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("round trip returned %q, want %q", got, data)
	}
}

// Identical content must occupy exactly one object regardless of how many
// times it is captured.
func TestPutContentDeduplicates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	data := []byte("an idle world_map screen")

	key1, sum1, err := PutContent(ctx, s, data)
	if err != nil {
		t.Fatalf("first PutContent(): %v", err)
	}
	key2, sum2, err := PutContent(ctx, s, data)
	if err != nil {
		t.Fatalf("second PutContent(): %v", err)
	}
	if key1 != key2 || sum1 != sum2 {
		t.Fatalf("same content produced different keys: %q/%q vs %q/%q", key1, sum1, key2, sum2)
	}

	var files int
	_ = filepath.Walk(s.root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Errorf("store holds %d files after two identical puts, want 1", files)
	}
}

func TestGetMissingKey(t *testing.T) {
	_, err := newTestStore(t).Get(context.Background(), "sha256/aa/bb/nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// Corrupt storage must surface as an error rather than silently returning
// bytes that no longer match their provenance record.
func TestGetContentDetectsCorruption(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	key, sum, err := PutContent(ctx, s, []byte("original"))
	if err != nil {
		t.Fatalf("PutContent(): %v", err)
	}

	full, err := s.path(key)
	if err != nil {
		t.Fatalf("path(): %v", err)
	}
	if err := os.WriteFile(full, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tampering with stored object: %v", err)
	}

	if _, err := GetContent(ctx, s, key, sum); err == nil {
		t.Fatal("GetContent() accepted tampered bytes, want digest mismatch error")
	} else if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error = %v, want a digest mismatch", err)
	}
}

// Traversal attempts are neutralised by rooting the key rather than rejected.
// What matters is that no key can ever resolve outside the store root.
func TestPathTraversalContained(t *testing.T) {
	s := newTestStore(t)
	root := filepath.Clean(s.root)

	for _, key := range []string{
		"../../etc/passwd",
		"sha256/../../../../etc/shadow",
		"/etc/passwd",
		`..\..\windows\system32`,
	} {
		full, err := s.path(key)
		if err != nil {
			continue // rejecting outright is also acceptable
		}
		if !strings.HasPrefix(full, root+string(os.PathSeparator)) {
			t.Errorf("key %q resolved to %q, which escapes root %q", key, full, root)
		}
	}
}
