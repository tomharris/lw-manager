package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("db: not found")

// Device is one adb target.
type Device struct {
	ID            int64
	Serial        string
	Transport     string
	ResolutionW   int
	ResolutionH   int
	Status        string
	LastHeartbeat *time.Time
}

// CaptureTarget is the joined account → app_instance → device view the
// capture path needs. Resolving it in one query keeps capture from having to
// know the shape of the ownership chain.
type CaptureTarget struct {
	AccountID   int64
	Nickname    string
	Role        string
	Enabled     bool
	Package     string
	CloneID     int
	DeviceID    int64
	Serial      string
	Transport   string
	ResolutionW int
	ResolutionH int
}

// Account is a game account pinned to an app instance.
type Account struct {
	ID            int64
	AppInstanceID int64
	GameUID       *string
	Nickname      string
	Server        *int
	Role          string
	Enabled       bool
}

// ValidRoles are the roles the scheduler understands. Mirrors the
// accounts_role_valid CHECK constraint; duplicated here so a bad --role gives
// a helpful message instead of a constraint violation.
var ValidRoles = []string{"main", "farm", "scout", "alliance_data"}

// IsValidRole reports whether role is one the scheduler understands.
func IsValidRole(role string) bool {
	for _, r := range ValidRoles {
		if r == role {
			return true
		}
	}
	return false
}

// Screenshot is a recorded capture.
type Screenshot struct {
	ID         int64
	AccountID  int64
	CapturedAt time.Time
	ScreenID   *string
	ObjectKey  string
	SHA256     string
}

// UpsertDevice registers a device or refreshes its resolution and status.
// Idempotent on serial so an agent can call it on every startup.
func (p *Pool) UpsertDevice(ctx context.Context, serial, transport string, w, h int) (Device, error) {
	const q = `
		INSERT INTO devices (serial, transport, resolution_w, resolution_h, status, last_heartbeat)
		VALUES ($1, $2, $3, $4, 'online', now())
		ON CONFLICT (serial) DO UPDATE SET
			transport      = EXCLUDED.transport,
			resolution_w   = EXCLUDED.resolution_w,
			resolution_h   = EXCLUDED.resolution_h,
			status         = 'online',
			last_heartbeat = now()
		RETURNING id, serial, transport, resolution_w, resolution_h, status, last_heartbeat`

	var d Device
	err := p.QueryRow(ctx, q, serial, transport, w, h).Scan(
		&d.ID, &d.Serial, &d.Transport, &d.ResolutionW, &d.ResolutionH, &d.Status, &d.LastHeartbeat)
	if err != nil {
		return Device{}, fmt.Errorf("db: upserting device %q: %w", serial, err)
	}
	return d, nil
}

// CaptureTargetByAccount resolves the full ownership chain for an account.
func (p *Pool) CaptureTargetByAccount(ctx context.Context, accountID int64) (CaptureTarget, error) {
	const q = `
		SELECT a.id, a.nickname, a.role, a.enabled,
		       ai.package, ai.clone_id,
		       d.id, d.serial, d.transport, d.resolution_w, d.resolution_h
		FROM accounts a
		JOIN app_instances ai ON ai.id = a.app_instance_id
		JOIN devices d       ON d.id  = ai.device_id
		WHERE a.id = $1`

	var t CaptureTarget
	err := p.QueryRow(ctx, q, accountID).Scan(
		&t.AccountID, &t.Nickname, &t.Role, &t.Enabled,
		&t.Package, &t.CloneID,
		&t.DeviceID, &t.Serial, &t.Transport, &t.ResolutionW, &t.ResolutionH)
	if errors.Is(err, pgx.ErrNoRows) {
		return CaptureTarget{}, fmt.Errorf("db: account %d: %w", accountID, ErrNotFound)
	}
	if err != nil {
		return CaptureTarget{}, fmt.Errorf("db: resolving capture target for account %d: %w", accountID, err)
	}
	return t, nil
}

// EnsureAppInstance finds or creates the app instance for a device, package
// and clone slot. Idempotent, so registration can be re-run safely.
func (p *Pool) EnsureAppInstance(ctx context.Context, deviceID int64, pkg string, cloneID int) (int64, error) {
	// DO UPDATE rather than DO NOTHING: DO NOTHING suppresses the RETURNING
	// row on conflict, which would leave id unset on a re-run.
	const q = `
		INSERT INTO app_instances (device_id, package, clone_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (device_id, package, clone_id) DO UPDATE SET device_id = EXCLUDED.device_id
		RETURNING id`

	var id int64
	if err := p.QueryRow(ctx, q, deviceID, pkg, cloneID).Scan(&id); err != nil {
		return 0, fmt.Errorf("db: ensuring app instance for device %d (%s clone %d): %w", deviceID, pkg, cloneID, err)
	}
	return id, nil
}

// UpsertAccount creates or updates the account bound to an app instance.
//
// One account per app instance is enforced by a UNIQUE constraint, so
// re-registering the same slot updates the existing account rather than
// silently creating a second one that could never be scheduled.
func (p *Pool) UpsertAccount(ctx context.Context, appInstanceID int64, nickname, role string, gameUID *string, server *int) (Account, error) {
	if !IsValidRole(role) {
		return Account{}, fmt.Errorf("db: invalid role %q, want one of %v", role, ValidRoles)
	}

	const q = `
		INSERT INTO accounts (app_instance_id, nickname, role, game_uid, server)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (app_instance_id) DO UPDATE SET
			nickname = EXCLUDED.nickname,
			role     = EXCLUDED.role,
			game_uid = COALESCE(EXCLUDED.game_uid, accounts.game_uid),
			server   = COALESCE(EXCLUDED.server, accounts.server)
		RETURNING id, app_instance_id, game_uid, nickname, server, role, enabled`

	var a Account
	err := p.QueryRow(ctx, q, appInstanceID, nickname, role, gameUID, server).Scan(
		&a.ID, &a.AppInstanceID, &a.GameUID, &a.Nickname, &a.Server, &a.Role, &a.Enabled)
	if err != nil {
		return Account{}, fmt.Errorf("db: upserting account %q on app instance %d: %w", nickname, appInstanceID, err)
	}
	return a, nil
}

// ListCaptureTargets returns every registered account with its device, for
// operator-facing listings.
func (p *Pool) ListCaptureTargets(ctx context.Context) ([]CaptureTarget, error) {
	const q = `
		SELECT a.id, a.nickname, a.role, a.enabled,
		       ai.package, ai.clone_id,
		       d.id, d.serial, d.transport, d.resolution_w, d.resolution_h
		FROM accounts a
		JOIN app_instances ai ON ai.id = a.app_instance_id
		JOIN devices d       ON d.id  = ai.device_id
		ORDER BY d.serial, ai.clone_id`

	rows, err := p.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: listing capture targets: %w", err)
	}
	defer rows.Close()

	var out []CaptureTarget
	for rows.Next() {
		var t CaptureTarget
		if err := rows.Scan(
			&t.AccountID, &t.Nickname, &t.Role, &t.Enabled,
			&t.Package, &t.CloneID,
			&t.DeviceID, &t.Serial, &t.Transport, &t.ResolutionW, &t.ResolutionH,
		); err != nil {
			return nil, fmt.Errorf("db: scanning capture target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// InsertScreenshot records one capture observation.
//
// Note this always inserts, even when sha256 matches an existing row: the blob
// is deduplicated by content address, but each capture is a distinct
// observation at a distinct time and must not be collapsed.
func (p *Pool) InsertScreenshot(ctx context.Context, accountID int64, objectKey, sha256 string, screenID *string) (Screenshot, error) {
	const q = `
		INSERT INTO screenshots (account_id, object_key, sha256, screen_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, account_id, captured_at, screen_id, object_key, sha256`

	var s Screenshot
	err := p.QueryRow(ctx, q, accountID, objectKey, sha256, screenID).Scan(
		&s.ID, &s.AccountID, &s.CapturedAt, &s.ScreenID, &s.ObjectKey, &s.SHA256)
	if err != nil {
		return Screenshot{}, fmt.Errorf("db: inserting screenshot for account %d: %w", accountID, err)
	}
	return s, nil
}
