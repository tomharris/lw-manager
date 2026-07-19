// Package db owns the Postgres schema and all SQL for the platform.
//
// Queries are hand-written against pgx rather than generated. At M0 the
// surface is four tables and a handful of statements, and a codegen step
// would add a toolchain dependency for very little leverage. Revisit if the
// query count grows past what is comfortable to read in one file.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgx connection pool.
type Pool struct {
	*pgxpool.Pool
}

// Connect opens a connection pool and verifies it with a ping, so a bad
// DATABASE_URL fails at startup rather than on first query.
func Connect(ctx context.Context, databaseURL string) (*Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: creating pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: pinging %s: %w", redact(databaseURL), err)
	}
	return &Pool{Pool: pool}, nil
}

// redact strips the password from a connection string before it reaches logs.
func redact(url string) string {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return "<unparseable database url>"
	}
	return fmt.Sprintf("%s@%s:%d/%s",
		cfg.ConnConfig.User, cfg.ConnConfig.Host, cfg.ConnConfig.Port, cfg.ConnConfig.Database)
}
