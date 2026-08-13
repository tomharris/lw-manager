package main

import (
	"context"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/config"
)

// connectReviewStore must degrade gracefully, never hang, and never return
// an error: corpus labelling is studio's original job and needs no
// database, so agent studio has to start regardless of whether Postgres is
// reachable. Pointed at a closed local port rather than a mock server, so
// this runs with no Docker and fails fast (connection refused) instead of
// waiting out reviewConnectTimeout.
func TestConnectReviewStoreDegradesGracefullyWhenTheDatabaseIsUnreachable(t *testing.T) {
	cfg := config.Config{
		DatabaseURL: "postgres://lw:lw@127.0.0.1:1/nope?sslmode=disable",
		Blob:        config.BlobConfig{Backend: config.BlobFS, FSRoot: t.TempDir()},
	}

	start := time.Now()
	pool, blobs := connectReviewStore(context.Background(), cfg)
	elapsed := time.Since(start)

	if pool != nil {
		t.Errorf("pool = %v, want nil when the database is unreachable", pool)
	}
	if blobs != nil {
		t.Errorf("blobs = %v, want nil alongside a nil pool", blobs)
	}
	if elapsed >= reviewConnectTimeout {
		t.Errorf("connectReviewStore took %s against a closed port, want a fast connection-refused failure well under the %s timeout", elapsed, reviewConnectTimeout)
	}
}
