package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
)

// KillSwitch answers "may this account act right now?". Checked by every Ctx
// primitive before it touches the device (invariant #8).
type KillSwitch interface {
	// Check returns nil, ErrPaused, ErrAccountDisabled, or a store error.
	Check(ctx context.Context) error
}

// KillStore is the slice of the database the switch needs. *db.Pool
// satisfies it; tests use a scripted fake.
type KillStore interface {
	KillState(ctx context.Context, accountID int64) (db.KillState, error)
}

// DBKillSwitch reads flags.pause_all and accounts.enabled, caching the
// answer briefly so a tight tap sequence does not hammer Postgres. The TTL
// is well inside the operational requirement of stopping everything within
// five seconds.
type DBKillSwitch struct {
	store     KillStore
	accountID int64
	ttl       time.Duration

	mu      sync.Mutex
	fetched time.Time
	cached  error
}

// NewDBKillSwitch builds a switch for one account with a 2s cache.
func NewDBKillSwitch(store KillStore, accountID int64) *DBKillSwitch {
	return &DBKillSwitch{store: store, accountID: accountID, ttl: 2 * time.Second}
}

func (k *DBKillSwitch) Check(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.fetched.IsZero() && time.Since(k.fetched) <= k.ttl {
		return k.cached
	}

	s, err := k.store.KillState(ctx, k.accountID)
	if err != nil {
		// Fail closed and uncached: if the database is unreachable we cannot
		// prove we are allowed to act, and the next check should retry.
		return fmt.Errorf("runtime: kill switch check for account %d: %w", k.accountID, err)
	}

	switch {
	case s.PauseAll:
		k.cached = fmt.Errorf("%w (reason: %s)", ErrPaused, s.Reason)
	case !s.AccountEnabled:
		k.cached = fmt.Errorf("account %d: %w", k.accountID, ErrAccountDisabled)
	default:
		k.cached = nil
	}
	k.fetched = time.Now()
	return k.cached
}
