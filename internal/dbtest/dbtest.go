// Package dbtest resolves the database that integration tests run against.
//
// It exists to keep tests off the development database. Tests deliberately do
// NOT honour LW_DATABASE_URL: that is the application's variable, and reusing
// it means a developer with it exported in their shell silently points the
// test suite at real data. Tests read LW_TEST_DATABASE_URL instead, and
// default to a separate database on the same server.
//
// This package imports no testing machinery so that it stays a plain library;
// callers turn its errors into t.Fatal themselves.
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DefaultURL is where integration tests look when LW_TEST_DATABASE_URL is
// unset. Port 5433 matches docker-compose; see the gotcha in CLAUDE.md.
const DefaultURL = "postgres://lw:lw@localhost:5433/lw_manager_test?sslmode=disable"

// requiredSuffix guards against a mistyped or copy-pasted LW_TEST_DATABASE_URL
// pointing at real data. Integration tests truncate and delete freely, so the
// cost of getting this wrong is losing the development database.
const requiredSuffix = "_test"

// ErrUnsafeDatabase reports a test database name that does not look like one.
var ErrUnsafeDatabase = errors.New("dbtest: refusing to run against a database not named *_test")

// migrationLockKey is an arbitrary constant identifying this project's schema
// migration. Postgres advisory locks are namespaced per database, so it only
// has to be unique within the test database.
const migrationLockKey = 0x6c776d67 // "lwmg"

// Prepare returns the integration-test database URL with the schema applied.
//
// The migrate function is passed in rather than imported so that dbtest does
// not depend on internal/db: the db package's own integration tests live in
// `package db`, and importing dbtest from there would be a cycle.
//
// Migration runs while holding a Postgres advisory lock. `go test ./...` runs
// package test binaries in parallel, so without it two processes race to apply
// the same migration against a freshly created database and one fails. That
// race only appears on a clean database — which is to say, on CI and on a new
// developer's first run.
func Prepare(ctx context.Context, migrate func(context.Context, string) error) (string, error) {
	url, err := URL(ctx)
	if err != nil {
		return "", err
	}

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return "", fmt.Errorf("dbtest: connecting to test database: %w", err)
	}
	defer conn.Close(ctx)

	// Session-level lock: released explicitly below, and by the server if this
	// process dies mid-migration.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLockKey)); err != nil {
		return "", fmt.Errorf("dbtest: acquiring migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(migrationLockKey))
	}()

	if err := migrate(ctx, url); err != nil {
		return "", fmt.Errorf("dbtest: migrating test database: %w", err)
	}
	return url, nil
}

// URL returns the integration-test database URL, creating the database if it
// does not exist yet. Safe to call from many tests: creation races resolve to
// "already exists", which is not an error here.
//
// Most callers want Prepare, which also applies the schema.
func URL(ctx context.Context) (string, error) {
	raw := os.Getenv("LW_TEST_DATABASE_URL")
	if raw == "" {
		raw = DefaultURL
	}

	name, err := databaseName(raw)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(name, requiredSuffix) {
		return "", fmt.Errorf("%w: got %q", ErrUnsafeDatabase, name)
	}

	if err := ensureDatabase(ctx, raw, name); err != nil {
		return "", err
	}
	return raw, nil
}

// databaseName extracts the database name from a postgres URL.
func databaseName(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("dbtest: parsing database url: %w", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return "", fmt.Errorf("dbtest: database url has no database name")
	}
	return name, nil
}

// ensureDatabase creates the test database if it is missing, by connecting to
// the server's maintenance database. CREATE DATABASE cannot run inside a
// transaction and cannot be parameterized, hence the identifier quoting.
func ensureDatabase(ctx context.Context, raw, name string) error {
	adminURL, err := withDatabase(raw, "postgres")
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("dbtest: connecting to maintenance database (is `docker compose up -d` running?): %w", err)
	}
	defer conn.Close(ctx)

	exists, err := databaseExists(ctx, conn, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, stmt); err != nil {
		// `go test ./...` starts package binaries in parallel, so two of them
		// reach CREATE DATABASE at once on a clean server. The loser does not
		// reliably get 42P04 (duplicate_database) — Postgres only reports that
		// when its own name lookup catches the conflict first. A true race
		// instead surfaces as 23505 from the unique index on pg_database.
		// Re-checking existence covers both without encoding SQLSTATEs.
		exists, checkErr := databaseExists(ctx, conn, name)
		if checkErr == nil && exists {
			return nil
		}
		return fmt.Errorf("dbtest: creating database %q: %w", name, err)
	}
	return nil
}

func databaseExists(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("dbtest: checking for database %q: %w", name, err)
	}
	return exists, nil
}

// withDatabase returns raw with its database name replaced.
func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("dbtest: parsing database url: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}
