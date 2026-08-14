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

// allianceKey is the same (tag, name) pair UpsertAlliance's ON CONFLICT
// target uses, and what fakeAllianceStore's rows map is keyed by.
type allianceKey struct{ tag, name string }

// fakeAllianceStore is a device- and database-free stand-in for the
// allianceStore surface runAlliance depends on — production satisfies it
// directly with *db.Pool (see runAllianceCmd), since all three methods
// already live there and no adapter is needed the way ingestService adapts
// two different types for runIngest.
//
// UpsertAlliance and AllianceByTagName are backed by a real (tag, name)-
// keyed map rather than a single canned return value: fix-round-1's review
// caught a bug that only a fake capable of holding two distinct alliances
// at once, and being queried back by key, could reproduce without a real
// database — see TestAllianceSetPreservesMemberCountWhenSwitchingBackToAPreviousIdentity.
type fakeAllianceStore struct {
	rows   map[allianceKey]db.Alliance
	nextID int64

	// current is what CurrentAlliance (used only by `show`) returns, or
	// currentErr if set. Deliberately independent of rows: CurrentAlliance
	// answers "most recently observed" in production, a different question
	// from AllianceByTagName's exact-key lookup, and the fake keeps that
	// same separation rather than deriving one from the other.
	current    db.Alliance
	currentErr error

	// upserted records the last UpsertAlliance call, so a test can assert
	// what was actually written (in particular, whether member_count was
	// carried forward rather than clobbered — see runAllianceSet).
	upserted    db.Alliance
	upsertCalls int
	upsertErr   error
}

func (f *fakeAllianceStore) CurrentAlliance(ctx context.Context) (db.Alliance, error) {
	if f.currentErr != nil {
		return db.Alliance{}, f.currentErr
	}
	return f.current, nil
}

func (f *fakeAllianceStore) AllianceByTagName(ctx context.Context, tag, name string) (db.Alliance, error) {
	if a, ok := f.rows[allianceKey{tag, name}]; ok {
		return a, nil
	}
	return db.Alliance{}, db.ErrNotFound
}

func (f *fakeAllianceStore) UpsertAlliance(ctx context.Context, a db.Alliance) (int64, error) {
	f.upserted = a
	f.upsertCalls++
	if f.upsertErr != nil {
		return 0, f.upsertErr
	}

	key := allianceKey{a.Tag, a.Name}
	if existing, ok := f.rows[key]; ok {
		a.ID = existing.ID // ON CONFLICT refreshes the existing row's id
	} else {
		f.nextID++
		a.ID = f.nextID
	}
	if f.rows == nil {
		f.rows = map[allianceKey]db.Alliance{}
	}
	f.rows[key] = a
	return a.ID, nil
}

func TestAllianceSetRejectsAMissingTag(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{}
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
	store := &fakeAllianceStore{}
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

// On a fresh deployment (no row for this tag+name yet) `set` must report
// the row as created and must not invent a member_count — there is nothing
// to carry forward yet.
func TestAllianceSetReportsCreatedOnAFreshDeployment(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa", "--name", "Organized Chaos"}, store)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"id=1", "action=created"} {
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
		rows: map[allianceKey]db.Alliance{
			{"OrCa", "Organized Chaos"}: {ID: 42, Tag: "OrCa", Name: "Organized Chaos", MemberCount: 97},
		},
	}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa", "--name", "Organized Chaos"}, store)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"id=42", "action=refreshed"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout %q missing %q", got, want)
		}
	}
	if store.upserted.MemberCount != 97 {
		t.Errorf("member_count passed to UpsertAlliance = %d, want 97 (carried forward from the existing row)", store.upserted.MemberCount)
	}
}

// This is the exact bug fix-round-1's review reproduced against a live
// database: deciding "created" vs "refreshed" (and what member_count to
// carry) by comparing against CurrentAlliance — "most recently observed" —
// instead of looking up the same (tag, name) key UpsertAlliance conflicts
// on. Two calls to `set` cannot expose this: CurrentAlliance and
// AllianceByTagName only disagree once a *second*, different alliance has
// ever been recorded, so this needs three — set A, set B (so B becomes
// "current" and A is no longer it), then set A again. Do not collapse this
// back to two calls; the regression it guards is invisible at two.
func TestAllianceSetPreservesMemberCountWhenSwitchingBackToAPreviousIdentity(t *testing.T) {
	store := &fakeAllianceStore{}

	var out, errOut bytes.Buffer
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", "A", "--name", "Alpha"}, store); code != 0 {
		t.Fatalf("set A: exit = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=created") {
		t.Errorf("set A stdout = %q, want action=created", out.String())
	}

	// Simulates ingest's SetAllianceMemberCount observing A's headcount from
	// the alliance frame's own "Members: 97/100" line, independently of
	// set — identity is declared, quantities are measured, and set itself
	// never carries a count in from a flag, so this reaches into the fake's
	// row directly rather than pretending set would do it.
	a := store.rows[allianceKey{"A", "Alpha"}]
	a.MemberCount = 97
	store.rows[allianceKey{"A", "Alpha"}] = a

	out.Reset()
	errOut.Reset()
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", "B", "--name", "Bravo"}, store); code != 0 {
		t.Fatalf("set B: exit = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=created") {
		t.Errorf("set B stdout = %q, want action=created", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runAlliance(&out, &errOut, []string{"set", "--tag", "A", "--name", "Alpha"}, store); code != 0 {
		t.Fatalf("set A again: exit = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "action=refreshed") {
		t.Errorf("set A again stdout = %q, want action=refreshed — A already existed", out.String())
	}
	if store.upserted.MemberCount != 97 {
		t.Errorf("member_count passed to UpsertAlliance when switching back to A = %d, want 97 (preserved, not zeroed)", store.upserted.MemberCount)
	}
}

func TestAllianceSetPassesServerThrough(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{}
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
	store := &fakeAllianceStore{upsertErr: errors.New("connection refused")}
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

// A failure checking for an existing row (anything other than
// ErrNotFound, which just means "nothing to carry forward") must abort
// before ever calling UpsertAlliance — an operator who sees this error
// should not also wonder whether a row was half-written.
func TestAllianceSetPropagatesALookupFailureWithoutUpserting(t *testing.T) {
	var out, errOut bytes.Buffer
	store := &fakeAllianceStore{}
	// AllianceByTagName has no direct error field on the fake (only
	// ErrNotFound via a miss); simulate a genuine lookup failure the same
	// way CurrentAlliance's currentErr does elsewhere, by wrapping the fake.
	failing := &lookupFailingAllianceStore{fakeAllianceStore: store, err: errors.New("connection refused")}
	code := runAlliance(&out, &errOut, []string{"set", "--tag", "OrCa", "--name", "Organized Chaos"}, failing)
	if code == 0 {
		t.Fatal("want a non-zero exit when the existing-alliance lookup fails")
	}
	if store.upsertCalls != 0 {
		t.Errorf("UpsertAlliance called %d times, want 0 — a lookup failure must not fall through to a write", store.upsertCalls)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should stay empty on this failure, got %q", out.String())
	}
}

// lookupFailingAllianceStore overrides AllianceByTagName to return an
// arbitrary error, for the one test above that needs to distinguish "no
// row found" (ErrNotFound, handled) from "the lookup itself broke"
// (anything else, must abort) — fakeAllianceStore's own AllianceByTagName
// can only report the former.
type lookupFailingAllianceStore struct {
	*fakeAllianceStore
	err error
}

func (l *lookupFailingAllianceStore) AllianceByTagName(ctx context.Context, tag, name string) (db.Alliance, error) {
	return db.Alliance{}, l.err
}
