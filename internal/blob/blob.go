// Package blob stores screenshot bytes under content-addressed keys.
//
// Every screenshot is keyed by the SHA-256 of its own bytes, which buys three
// things at once: identical frames cost storage only once, the key doubles as
// an integrity check, and a screenshots row can always be traced back to the
// exact pixels it was derived from. That last property is what makes an
// OCR-derived number defensible when a member disputes it.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// ErrNotFound is returned by Get when a key holds no object.
var ErrNotFound = errors.New("blob: not found")

// Store is the object store abstraction. FSStore backs dev and tests; S3Store
// backs the compose stack.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Exists(ctx context.Context, key string) (bool, error)
}

// Sum returns the hex SHA-256 of data.
func Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Key builds the object key for a hex digest.
//
// The two-level fan-out ("sha256/ab/cd/abcd...") keeps any single directory
// small on the filesystem backend. A busy fleet writes tens of thousands of
// screenshots, and a flat directory degrades badly at that scale.
func Key(sum string) string {
	if len(sum) < 4 {
		return "sha256/" + sum
	}
	return fmt.Sprintf("sha256/%s/%s/%s", sum[0:2], sum[2:4], sum)
}

// PutContent hashes data, writes it if absent, and returns its key and digest.
//
// The Exists check makes repeated captures of an unchanged screen nearly free,
// which matters because polling loops capture the same idle screen constantly.
func PutContent(ctx context.Context, s Store, data []byte) (key, sum string, err error) {
	sum = Sum(data)
	key = Key(sum)

	exists, err := s.Exists(ctx, key)
	if err != nil {
		return "", "", fmt.Errorf("blob: checking %s: %w", key, err)
	}
	if exists {
		return key, sum, nil
	}

	if err := s.Put(ctx, key, newByteReader(data)); err != nil {
		return "", "", fmt.Errorf("blob: writing %s: %w", key, err)
	}
	return key, sum, nil
}

// GetContent reads a whole object and verifies it against its key's digest.
// A mismatch means the store is corrupt and callers must not trust the bytes.
func GetContent(ctx context.Context, s Store, key, wantSum string) ([]byte, error) {
	rc, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("blob: reading %s: %w", key, err)
	}
	if got := Sum(data); got != wantSum {
		return nil, fmt.Errorf("blob: %s digest mismatch: stored bytes hash to %s, want %s", key, got, wantSum)
	}
	return data, nil
}
