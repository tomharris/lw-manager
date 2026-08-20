//go:build m4gate || m4rostergate

// Helpers shared by the two M4 gates. They live behind a disjunction of both
// tags rather than in either gate's own file because each gate is compiled
// alone: -tags m4gate does not build the roster gate and vice versa, so a
// helper declared in one is invisible to the other and declaring it in both
// collides when someone builds with both.
package ingest_test

import (
	"context"
	"os"
	"testing"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/dbtest"
)

func gatePool(t *testing.T, ctx context.Context) *db.Pool {
	t.Helper()
	url, err := dbtest.Prepare(ctx, db.Migrate)
	if err != nil {
		t.Fatalf("dbtest.Prepare(): %v", err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func join(lines []string) string {
	out := ""
	for _, l := range lines {
		out += "  " + l + "\n"
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
