//go:build integration

package db

import (
	"context"
	"errors"
	"os"
	"testing"
)

func testPool(t *testing.T) *Pool {
	t.Helper()
	url := os.Getenv("LW_DATABASE_URL")
	if url == "" {
		url = "postgres://lw:lw@localhost:5433/lw_manager?sslmode=disable"
	}
	ctx := context.Background()
	if err := Migrate(ctx, url); err != nil {
		t.Fatalf("Migrate(): %v", err)
	}
	p, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Exercises the whole ownership chain: device → app_instance → account →
// screenshot, which is exactly the path internal/capture walks.
func TestCapturePathRoundTrip(t *testing.T) {
	ctx := context.Background()
	p := testPool(t)

	// Clean slate; cascades take the dependent rows with it.
	if _, err := p.Exec(ctx, `DELETE FROM devices WHERE serial LIKE 'test-%'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	dev, err := p.UpsertDevice(ctx, "test-emulator-5554", "adb", 1080, 2400)
	if err != nil {
		t.Fatalf("UpsertDevice(): %v", err)
	}
	if dev.Status != "online" {
		t.Errorf("status = %q, want online", dev.Status)
	}

	// Upsert must be idempotent: same serial, same row, updated resolution.
	dev2, err := p.UpsertDevice(ctx, "test-emulator-5554", "adb", 1080, 1920)
	if err != nil {
		t.Fatalf("UpsertDevice() second call: %v", err)
	}
	if dev2.ID != dev.ID {
		t.Errorf("upsert created a new row: %d then %d", dev.ID, dev2.ID)
	}
	if dev2.ResolutionH != 1920 {
		t.Errorf("resolution_h = %d, want 1920", dev2.ResolutionH)
	}

	var appInstanceID, accountID int64
	if err := p.QueryRow(ctx,
		`INSERT INTO app_instances (device_id) VALUES ($1) RETURNING id`, dev.ID,
	).Scan(&appInstanceID); err != nil {
		t.Fatalf("insert app_instance: %v", err)
	}
	if err := p.QueryRow(ctx,
		`INSERT INTO accounts (app_instance_id, nickname, role) VALUES ($1, 'testalt', 'alliance_data') RETURNING id`,
		appInstanceID,
	).Scan(&accountID); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	target, err := p.CaptureTargetByAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("CaptureTargetByAccount(): %v", err)
	}
	if target.Serial != "test-emulator-5554" || target.Role != "alliance_data" {
		t.Errorf("target = %+v, want serial test-emulator-5554 role alliance_data", target)
	}
	if target.Package != "com.fun.lastwar.gp" {
		t.Errorf("package = %q, want the default game package", target.Package)
	}

	const sum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	s1, err := p.InsertScreenshot(ctx, accountID, "sha256/"+sum, sum, nil)
	if err != nil {
		t.Fatalf("InsertScreenshot(): %v", err)
	}
	if s1.ScreenID != nil {
		t.Errorf("screen_id = %v, want NULL at M0", *s1.ScreenID)
	}

	// Identical content captured twice is two observations, one blob.
	s2, err := p.InsertScreenshot(ctx, accountID, "sha256/"+sum, sum, nil)
	if err != nil {
		t.Fatalf("InsertScreenshot() duplicate content: %v", err)
	}
	if s2.ID == s1.ID {
		t.Error("duplicate content collapsed into one screenshot row; each capture is a distinct observation")
	}
	if s2.ObjectKey != s1.ObjectKey {
		t.Errorf("object keys differ for identical content: %q vs %q", s1.ObjectKey, s2.ObjectKey)
	}
}

func TestCaptureTargetNotFound(t *testing.T) {
	p := testPool(t)
	_, err := p.CaptureTargetByAccount(context.Background(), 999999999)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// The sha256 CHECK constraint is the last line of defence against a truncated
// or uppercase digest reaching the facts table via provenance links.
func TestScreenshotRejectsMalformedDigest(t *testing.T) {
	ctx := context.Background()
	p := testPool(t)
	if _, err := p.InsertScreenshot(ctx, 1, "sha256/nope", "NOT-A-DIGEST", nil); err == nil {
		t.Fatal("insert with a malformed sha256 succeeded, want constraint violation")
	}
}

// Registration must be safe to re-run: an operator will run it again after a
// typo, or after the emulator is recreated with the same serial.
func TestRegistrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	p := testPool(t)

	if _, err := p.Exec(ctx, `DELETE FROM devices WHERE serial = 'test-register'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	register := func(nickname, role string) (int64, int64, Account) {
		t.Helper()
		dev, err := p.UpsertDevice(ctx, "test-register", "adb", 1080, 2400)
		if err != nil {
			t.Fatalf("UpsertDevice(): %v", err)
		}
		inst, err := p.EnsureAppInstance(ctx, dev.ID, "com.fun.lastwar.gp", 0)
		if err != nil {
			t.Fatalf("EnsureAppInstance(): %v", err)
		}
		acct, err := p.UpsertAccount(ctx, inst, nickname, role, nil, nil)
		if err != nil {
			t.Fatalf("UpsertAccount(): %v", err)
		}
		return dev.ID, inst, acct
	}

	dev1, inst1, acct1 := register("firstname", "farm")
	dev2, inst2, acct2 := register("corrected", "alliance_data")

	if dev1 != dev2 {
		t.Errorf("re-registering created a second device: %d then %d", dev1, dev2)
	}
	if inst1 != inst2 {
		t.Errorf("re-registering created a second app instance: %d then %d", inst1, inst2)
	}
	if acct1.ID != acct2.ID {
		t.Errorf("re-registering created a second account: %d then %d", acct1.ID, acct2.ID)
	}
	if acct2.Nickname != "corrected" || acct2.Role != "alliance_data" {
		t.Errorf("re-registration did not apply corrections: %+v", acct2)
	}

	// A second clone slot on the same device is a genuinely different account.
	inst3, err := p.EnsureAppInstance(ctx, dev1, "com.fun.lastwar.gp", 1)
	if err != nil {
		t.Fatalf("EnsureAppInstance() clone 1: %v", err)
	}
	if inst3 == inst1 {
		t.Error("clone slot 1 reused the app instance for clone slot 0")
	}
	acct3, err := p.UpsertAccount(ctx, inst3, "secondalt", "farm", nil, nil)
	if err != nil {
		t.Fatalf("UpsertAccount() on clone 1: %v", err)
	}
	if acct3.ID == acct1.ID {
		t.Error("a distinct clone slot reused the first account")
	}

	// The registered accounts must be resolvable by the capture path.
	target, err := p.CaptureTargetByAccount(ctx, acct3.ID)
	if err != nil {
		t.Fatalf("CaptureTargetByAccount(): %v", err)
	}
	if target.CloneID != 1 || target.Serial != "test-register" {
		t.Errorf("target = %+v, want clone 1 on test-register", target)
	}
}

func TestUpsertAccountRejectsUnknownRole(t *testing.T) {
	ctx := context.Background()
	p := testPool(t)
	if _, err := p.UpsertAccount(ctx, 1, "x", "overlord", nil, nil); err == nil {
		t.Fatal("UpsertAccount() accepted an unknown role")
	}
}
