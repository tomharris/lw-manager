package dbtest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// seedSeq disambiguates seed rows created within the same nanosecond, so
// repeated `make test-integration` runs against a database that is never
// dropped between invocations never collide on the devices.serial UNIQUE
// constraint. captures.account_id has no ON DELETE CASCADE, so cleaning up a
// stale account by serial is not an option once a capture references it;
// minting a fresh serial per call sidesteps that entirely.
var seedSeq int64

func uniqueSuffix() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddInt64(&seedSeq, 1))
}

// querier is the one capability the seed helpers need from *db.Pool. It is
// declared locally, rather than importing *db.Pool from internal/db, because
// internal/db's own integration tests live in package db (not db_test) and
// import dbtest — importing internal/db back from here would be a cycle.
// *db.Pool satisfies this structurally via its embedded *pgxpool.Pool.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SeedAccount inserts the device, app instance and account rows an analytics
// test needs, and returns the account id. Integration tests truncate freely,
// so this is called per test rather than once.
func SeedAccount(ctx context.Context, t *testing.T, pool querier) int64 {
	t.Helper()
	suffix := uniqueSuffix()

	var deviceID, instanceID, accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO devices (serial, transport, resolution_w, resolution_h, status)
		 VALUES ($1, 'adb', 720, 1600, 'online') RETURNING id`,
		"TESTSERIAL-"+suffix).Scan(&deviceID); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO app_instances (device_id, package) VALUES ($1, 'com.fun.lastwar.gp') RETURNING id`,
		deviceID).Scan(&instanceID); err != nil {
		t.Fatalf("seed app instance: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (app_instance_id, nickname, role, enabled)
		 VALUES ($1, $2, 'alliance_data', true) RETURNING id`,
		instanceID, "testalt-"+suffix).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return accountID
}

// SeedScreenshot inserts one screenshot row and returns its id.
func SeedScreenshot(ctx context.Context, t *testing.T, pool querier, accountID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO screenshots (account_id, captured_at, object_key, sha256)
		 VALUES ($1, now(), 'test/key.png', repeat('a', 64)) RETURNING id`,
		accountID).Scan(&id); err != nil {
		t.Fatalf("seed screenshot: %v", err)
	}
	return id
}
