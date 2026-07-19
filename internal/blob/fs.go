package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FSStore keeps objects as files under a root directory. Used for local dev
// and for every test that needs a store, so tests need no running MinIO.
type FSStore struct {
	root string
}

// NewFSStore creates the root directory if it does not exist.
func NewFSStore(root string) (*FSStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("blob: creating fs root %s: %w", root, err)
	}
	return &FSStore{root: root}, nil
}

// path resolves a key to a filesystem path, rejecting keys that would escape
// the root. Keys are generated internally today, but this is the kind of check
// that must exist before any externally-influenced key ever reaches it.
func (s *FSStore) path(key string) (string, error) {
	clean := filepath.Clean("/" + strings.ReplaceAll(key, "\\", "/"))
	full := filepath.Join(s.root, clean)
	if !strings.HasPrefix(full, filepath.Clean(s.root)+string(os.PathSeparator)) {
		return "", fmt.Errorf("blob: key %q escapes the store root", key)
	}
	return full, nil
}

func (s *FSStore) Put(ctx context.Context, key string, r io.Reader) error {
	full, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("blob: creating directory for %s: %w", key, err)
	}

	// Write to a temp file and rename, so a crash mid-write can never leave a
	// truncated object sitting at a content-addressed key — which would fail
	// its digest check forever after.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return fmt.Errorf("blob: creating temp file for %s: %w", key, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return fmt.Errorf("blob: writing %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blob: closing temp file for %s: %w", key, err)
	}
	if err := os.Rename(tmp.Name(), full); err != nil {
		return fmt.Errorf("blob: committing %s: %w", key, err)
	}
	return nil
}

func (s *FSStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("blob: %s: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: opening %s: %w", key, err)
	}
	return f, nil
}

func (s *FSStore) Exists(ctx context.Context, key string) (bool, error) {
	full, err := s.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("blob: stat %s: %w", key, err)
	}
	return true, nil
}

func newByteReader(b []byte) io.Reader { return bytes.NewReader(b) }
