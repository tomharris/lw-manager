package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
)

// fakeAllianceStore is a device- and database-free stand-in for the
// allianceStore surface runAlliance depends on — production satisfies it
// directly with *db.Pool (see runAllianceCmd), since both methods already
// live there and no adapter is needed the way ingestService adapts two
// different types for runIngest.
type fakeAllianceStore struct {
	// current is what CurrentAlliance returns, or currentErr if set.
	current    db.Alliance
	currentErr error

	// upserted records the last UpsertAlliance call, so a test can assert
	// what was actually written (in particular, whether member_count was
	// carried forward rather than clobbered — see runAllianceSet).
	upserted    db.Alliance
	upsertCalls int
	upsertID    int64
	upsertErr   error
}

func (f *fakeAllianceStore) CurrentAlliance(ctx context.Context) (db.Alliance, error) {
	if f.currentErr != nil {
		return db.Alliance{}, f.currentErr
	}
	return f.current, nil
}

func (f *fakeAllianceStore) UpsertAlliance(ctx context.Context, a db.Alliance) (int64, error) {
	f.upserted = a
	f.upsertCalls++
	if f.upsertErr != nil {
		return 0, f.upsertErr
	}
	if f.upsertID != 0 {
		return f.upsertID, nil
	}
	return 1, nil
}

func TestAllianceSetRejectsAMissingTag(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{currentErr: db.ErrNotFound}
	code := runAlliance(&out, &errOut, []string{"set", "--name", "Organized Chaos"}, store)
	if code == 0 {
		t.Fatal("want a non-zero exit when --tag is missing")
	}
	if !strings.Contains(errOut.String(), "--tag") {
		t.Errorf("errOut = %q, want it to name the missing --tag flag", errOut.String())
	}
	if store.upsertCalls != 0 {
		t.Errorf("UpsertAlliance called %d times, want 0 — a usage error must not touch the store", store.upsertCalls)
	}
}

func TestAllianceSetRejectsAMissingName(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{currentErr: db.ErrNotFound}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa"}, store)
	if code == 0 {
		t.Fatal("want a non-zero exit when --name is missing")
	}
	if !strings.Contains(errOut.String(), "--name") {
		t.Errorf("errOut = %q, want it to name the missing --name flag", errOut.String())
	}
	if store.upsertCalls != 0 {
		t.Errorf("UpsertAlliance called %d times, want 0 — a usage error must not touch the store", store.upsertCalls)
	}
}

// On a fresh deployment (no current alliance yet) `set` must report the row
// as created and must not invent a member_count — there is nothing to carry
// forward yet.
func TestAllianceSetReportsCreatedOnAFreshDeployment(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{currentErr: db.ErrNotFound, upsertID: 42}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa", "--name", "Organized Chaos"}, store)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"id=42", "action=created"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout %q missing %q", got, want)
		}
	}
	if store.upserted.MemberCount != 0 {
		t.Errorf("member_count passed to UpsertAlliance = %d, want 0 on a fresh deployment", store.upserted.MemberCount)
	}
}

// Re-running `set` with the same tag+name must report the row as refreshed,
// and — this is the part that is easy to get wrong — must carry the
// existing member_count forward rather than clobbering it with zero.
// UpsertAlliance's ON CONFLICT overwrites member_count with whatever value
// it is given, and identity (`set`) must not stomp a quantity that ingest
// (`SetAllianceMemberCount`) already observed, per this task's own "identity
// is declared, quantities are measured" split.
func TestAllianceSetPreservesTheObservedMemberCountOnRefresh(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{
		current: db.Alliance{ID: 42, Tag: "OrCa", Name: "Organized Chaos", MemberCount: 97},
	}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa", "--name", "Organized Chaos"}, store)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=refreshed") {
		t.Errorf("stdout = %q, want action=refreshed", out.String())
	}
	if store.upserted.MemberCount != 97 {
		t.Errorf("member_count passed to UpsertAlliance = %d, want 97 (carried forward from the existing row)", store.upserted.MemberCount)
	}
}

// A different tag+name than whatever is currently recorded is a genuinely
// new alliance identity (or the first one, if none has been set), so it must
// not inherit the previous alliance's member_count.
func TestAllianceSetReportsCreatedForADifferentTagOrName(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{
		current:  db.Alliance{ID: 1, Tag: "OLD", Name: "Old Alliance", MemberCount: 50},
		upsertID: 2,
	}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa", "--name", "Organized Chaos"}, store)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=created") {
		t.Errorf("stdout = %q, want action=created", out.String())
	}
	if store.upserted.MemberCount != 0 {
		t.Errorf("member_count passed to UpsertAlliance = %d, want 0 — a distinct identity must not inherit another alliance's count", store.upserted.MemberCount)
	}
}

func TestAllianceSetPassesServerThrough(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{currentErr: db.ErrNotFound}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa", "--name", "Organized Chaos", "--server", "1380"}, store)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if store.upserted.Server != "1380" {
		t.Errorf("server passed to UpsertAlliance = %q, want 1380", store.upserted.Server)
	}
}

func TestAllianceShowPrintsTheCurrentAlliance(t *testing.T) {
	var out, errOut bytes.Buffer
	observed := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	store := &fakeAllianceStore{current: db.Alliance{
		ID: 42, Tag: "OrCa", Name: "Organized Chaos", Server: "1380", MemberCount: 97, ObservedAt: observed,
	}}
	code := runAlliance(&out, &errOut, []string{"show"}, store)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"id=42", "OrCa", "Organized Chaos", "1380", "member_count=97", "2026-08-12"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout %q missing %q", got, want)
		}
	}
}

// show on an empty table is the exact failure this task exists to make
// legible: a user who hits the ingest error needs to be told what to run,
// and show is where they will look.
func TestAllianceShowOnAnEmptyTableNamesTheFix(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{currentErr: db.ErrNotFound}
	code := runAlliance(&out, &errOut, []string{"show"}, store)
	if code == 0 {
		t.Fatal("want a non-zero exit when no alliance has ever been recorded")
	}
	if out.Len() != 0 {
		t.Errorf("stdout should stay empty on this failure, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "control alliance set") {
		t.Errorf("errOut = %q, want it to name `control alliance set`", errOut.String())
	}
}

func TestAllianceRejectsAnUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{}
	code := runAlliance(&out, &errOut, []string{"delete"}, store)
	if code == 0 {
		t.Fatal("want a non-zero exit for an unknown subcommand")
	}
	if store.upsertCalls != 0 {
		t.Errorf("UpsertAlliance called %d times, want 0", store.upsertCalls)
	}
}

func TestAllianceRejectsNoSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{}
	code := runAlliance(&out, &errOut, nil, store)
	if code == 0 {
		t.Fatal("want a non-zero exit when no subcommand is given")
	}
}

func TestAllianceSetPropagatesAnUpsertFailure(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{currentErr: db.ErrNotFound, upsertErr: errors.New("connection refused")}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa", "--name", "Organized Chaos"}, store)
	if code == 0 {
		t.Fatal("want a non-zero exit when the upsert fails")
	}
	if out.Len() != 0 {
		t.Errorf("stdout should stay empty on a failed set, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "level=ERROR") {
		t.Errorf("errOut = %q, want the failure logged there", errOut.String())
	}
}
