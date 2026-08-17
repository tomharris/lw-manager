//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/dbtest"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	ctx := context.Background()
	url, err := dbtest.Prepare(ctx, db.Migrate)
	if err != nil {
		t.Fatalf("dbtest.Prepare(): %v", err)
	}
	p, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("db.Connect(): %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// testSuffix mints a value unique to this call, mirroring
// internal/db/analytics_integration_test.go's own helper: the shared test
// database is never dropped between `make test-integration` runs, so a
// literal tag like "OrCa" would collide with a row an earlier run left
// behind.
func testSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// This is the property that makes `control alliance set` safe to re-run: an
// operator who is not sure whether it already ran (after a crash, a flaky
// connection, or just habit) can run it again and get back the same row,
// not a second alliance splitting facts between two identities — see this
// task's brief for why that split is dangerous specifically because every
// individual fact still traces correctly to its screenshot.
func TestAllianceSetAgainstARealDatabaseIsIdempotentAndDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	tag := "IDM-" + suffix
	name := "Idempotency Test " + suffix

	var out, errOut bytes.Buffer
	code := runAlliance(&out, &errOut, []string{"set", "--tag", tag, "--name", name}, pool)
	if code != 0 {
		t.Fatalf("first set: exit = %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=created") {
		t.Errorf("first set stdout = %q, want action=created", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = runAlliance(&out, &errOut, []string{"set", "--tag", tag, "--name", name}, pool)
	if code != 0 {
		t.Fatalf("second set: exit = %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=refreshed") {
		t.Errorf("second set stdout = %q, want action=refreshed", out.String())
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alliances WHERE tag = $1 AND name = $2`, tag, name,
	).Scan(&count); err != nil {
		t.Fatalf("counting alliance rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("alliance rows for (%s, %s) = %d, want exactly 1 — a re-run must refresh, not duplicate", tag, name, count)
	}
}

// A re-run of `set` must not clobber the member_count ingest already wrote
// via SetAllianceMemberCount. This is the real-database counterpart to
// TestAllianceSetPreservesTheObservedMemberCountOnRefresh in
// alliance_test.go — that one proves runAllianceSet passes the right value
// to UpsertAlliance; this one proves UpsertAlliance's ON CONFLICT actually
// persists it rather than the two disagreeing about what "the right value"
// means at the SQL layer.
func TestAllianceSetAgainstARealDatabasePreservesAnObservedMemberCount(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	tag := "MC-" + suffix
	name := "Member Count Test " + suffix

	var out, errOut bytes.Buffer
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", tag, "--name", name}, pool); code != 0 {
		t.Fatalf("set: exit = %d, want 0: %s", code, errOut.String())
	}

	allianceID, err := pool.CurrentAllianceID(ctx)
	if err != nil {
		t.Fatalf("CurrentAllianceID: %v", err)
	}
	// Simulates what ingest does on every roster capture: write the observed
	// "Members: 97/100" count by id, independently of set.
	if err := pool.SetAllianceMemberCount(ctx, allianceID, 97); err != nil {
		t.Fatalf("SetAllianceMemberCount: %v", err)
	}

	out.Reset()
	errOut.Reset()
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", tag, "--name", name}, pool); code != 0 {
		t.Fatalf("second set: exit = %d, want 0: %s", code, errOut.String())
	}

	var got int
	if err := pool.QueryRow(ctx, `SELECT member_count FROM alliances WHERE id = $1`, allianceID).Scan(&got); err != nil {
		t.Fatalf("reading back member_count: %v", err)
	}
	if got != 97 {
		t.Fatalf("member_count after re-running set = %d, want 97 (the observed count set carried forward, not clobbered)", got)
	}
}

// The real-database counterpart to
// TestAllianceSetPreservesMemberCountWhenSwitchingBackToAPreviousIdentity in
// alliance_test.go: fix-round-1's review reproduced this bug against a live
// database, so it belongs here too, exercising the actual AllianceByTagName
// query and UpsertAlliance's actual ON CONFLICT — not just the in-memory
// fake's model of them. Needs three set calls across two distinct
// alliances; the two-call idempotency test above cannot see this bug, since
// CurrentAlliance and AllianceByTagName only disagree once a second alliance
// has ever been recorded. Do not collapse this back to two calls.
func TestAllianceSetAgainstARealDatabasePreservesMemberCountWhenSwitchingBackToAPreviousIdentity(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	suffix := testSuffix()
	tagA, nameA := "SWA-"+suffix, "Switch A "+suffix
	tagB, nameB := "SWB-"+suffix, "Switch B "+suffix

	var out, errOut bytes.Buffer
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", tagA, "--name", nameA}, pool); code != 0 {
		t.Fatalf("set A: exit = %d, want 0: %s", code, errOut.String())
	}

	allianceA, err := pool.AllianceByTagName(ctx, tagA, nameA)
	if err != nil {
		t.Fatalf("AllianceByTagName(A): %v", err)
	}
	// Simulates what ingest does on every roster capture for A, before B is
	// ever set — independently of set, per "identity is declared, quantities
	// are measured".
	if err := pool.SetAllianceMemberCount(ctx, allianceA.ID, 97); err != nil {
		t.Fatalf("SetAllianceMemberCount(A, 97): %v", err)
	}

	out.Reset()
	errOut.Reset()
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", tagB, "--name", nameB}, pool); code != 0 {
		t.Fatalf("set B: exit = %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=created") {
		t.Errorf("set B stdout = %q, want action=created", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", tagA, "--name", nameA}, pool); code != 0 {
		t.Fatalf("set A again: exit = %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=refreshed") {
		t.Errorf("set A again stdout = %q, want action=refreshed — A already existed", out.String())
	}

	var got int
	if err := pool.QueryRow(ctx, `SELECT member_count FROM alliances WHERE id = $1`, allianceA.ID).Scan(&got); err != nil {
		t.Fatalf("reading back A's member_count: %v", err)
	}
	if got != 97 {
		t.Fatalf("A's member_count after switching to B and back = %d, want 97 (preserved, not zeroed by the bug fix-round-1 caught)", got)
	}
}

// show against the row set actually wrote — the two subcommands driven
// together through a real database, the way an operator would use them.
func TestAllianceShowAgainstARealDatabasePrintsWhatSetWrote(t *testing.T) {
	pool := testPool(t)

	suffix := testSuffix()
	tag := "SHW-" + suffix
	name := "Show Test " + suffix

	var out, errOut bytes.Buffer
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", tag, "--name", name, "--server", "1380"}, pool); code != 0 {
		t.Fatalf("set: exit = %d, want 0: %s", code, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runAlliance(&out, &errOut, []string{"show"}, pool); code != 0 {
		t.Fatalf("show: exit = %d, want 0: %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{tag, name, "1380", "member_count=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("show stdout %q missing %q", got, want)
		}
	}
}
