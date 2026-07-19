//go:build integration

package blob

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/config"
)

// Runs S3Store through the same contract FSStore satisfies, so the two
// backends cannot quietly diverge.
func TestS3StoreContract(t *testing.T) {
	ctx := context.Background()
	cfg := config.BlobConfig{
		Backend:     config.BlobS3,
		S3Endpoint:  "localhost:9000",
		S3AccessKey: "minioadmin",
		S3SecretKey: "minioadmin",
		S3Bucket:    "screenshots-test",
	}

	s, err := NewS3Store(ctx, cfg)
	if err != nil {
		t.Fatalf("NewS3Store(): %v", err)
	}

	data := []byte("fake png bytes for s3")
	key, sum, err := PutContent(ctx, s, data)
	if err != nil {
		t.Fatalf("PutContent(): %v", err)
	}

	got, err := GetContent(ctx, s, key, sum)
	if err != nil {
		t.Fatalf("GetContent(): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("round trip returned %q, want %q", got, data)
	}

	exists, err := s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists(): %v", err)
	}
	if !exists {
		t.Error("Exists() = false for a key just written")
	}

	if _, err := s.Get(ctx, "sha256/aa/bb/definitely-not-here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() on a missing key = %v, want ErrNotFound", err)
	}
}
