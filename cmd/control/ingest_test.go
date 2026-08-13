package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/ingest"
)

// callLog records dispatch evidence across fakeIngester's value-receiver
// methods. runIngest is given a fakeIngester by value (matching the literal
// call shape the task brief specifies), so a pointer field is what lets a
// test observe which route actually ran without changing that shape.
type callLog struct {
	rosterCalls  int
	rosterPeriod string
	vsCalls      int
	vsPeriod     string
}

// fakeIngester is a device- and database-free stand-in for ingestService. Its
// Capture method defaults to the vs_ranking route when route is left unset,
// which is what lets the task brief's own literal
// (fakeIngester{matched: 94, queued: 2, zeroed: 2} — Zeroed only exists on
// VSResult) dispatch to a real route without naming one.
type fakeIngester struct {
	route     string
	startedAt time.Time
	status    string

	matched, created, queued, zeroed int
	perGroup                         map[string]ingest.GroupTally

	captureErr error
	rosterErr  error
	vsErr      error

	calls *callLog
}

func (f fakeIngester) Capture(ctx context.Context, id int64) (db.Capture, error) {
	if f.captureErr != nil {
		return db.Capture{}, f.captureErr
	}
	route := f.route
	if route == "" {
		route = "vs_ranking"
	}
	status := f.status
	if status == "" {
		status = "complete"
	}
	return db.Capture{ID: id, Route: route, StartedAt: f.startedAt, Status: status}, nil
}

func (f fakeIngester) IngestRoster(ctx context.Context, captureID int64, periodKey string) (ingest.RosterResult, error) {
	if f.calls != nil {
		f.calls.rosterCalls++
		f.calls.rosterPeriod = periodKey
	}
	if f.rosterErr != nil {
		return ingest.RosterResult{}, f.rosterErr
	}
	status := f.status
	if status == "" {
		status = "complete"
	}
	return ingest.RosterResult{
		Matched: f.matched, Created: f.created, Queued: f.queued,
		Status: status, PerGroup: f.perGroup,
	}, nil
}

func (f fakeIngester) IngestVS(ctx context.Context, captureID int64, periodKey string) (ingest.VSResult, error) {
	if f.calls != nil {
		f.calls.vsCalls++
		f.calls.vsPeriod = periodKey
	}
	if f.vsErr != nil {
		return ingest.VSResult{}, f.vsErr
	}
	status := f.status
	if status == "" {
		status = "complete"
	}
	return ingest.VSResult{Matched: f.matched, Queued: f.queued, Zeroed: f.zeroed, Status: status}, nil
}

func TestIngestPrintsASummaryToStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runIngest(&out, &errOut, []string{"--capture", "7"}, fakeIngester{matched: 94, queued: 2, zeroed: 2})
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "94") {
		t.Errorf("stdout missing the matched count: %q", out.String())
	}
	if strings.Contains(out.String(), "level=") {
		t.Error("log output leaked into stdout — results must stay pipeable")
	}
}

func TestIngestRejectsAMissingCaptureFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runIngest(&out, &errOut, nil, fakeIngester{}); code == 0 {
		t.Fatal("want a non-zero exit when --capture is absent")
	}
}

func TestIngestRejectsAnUnparseableCaptureFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runIngest(&out, &errOut, []string{"--capture", "not-a-number"}, fakeIngester{})
	if code == 0 {
		t.Fatal("want a non-zero exit when --capture is not a number")
	}
	if out.Len() != 0 {
		t.Errorf("stdout should stay empty on a usage error, got %q", out.String())
	}
}

func TestIngestDispatchesTheRosterRouteFromTheCaptureRow(t *testing.T) {
	var out, errOut bytes.Buffer
	calls := &callLog{}
	svc := fakeIngester{
		route: "roster", status: "partial",
		matched: 90, created: 3, queued: 3,
		perGroup: map[string]ingest.GroupTally{"R2": {Expected: 20, Parsed: 18}},
		calls:    calls,
	}
	code := runIngest(&out, &errOut, []string{"--capture", "42"}, svc)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if calls.rosterCalls != 1 || calls.vsCalls != 0 {
		t.Fatalf("calls = %+v, want exactly one IngestRoster call and zero IngestVS calls", calls)
	}
	got := out.String()
	for _, want := range []string{"route=roster", "matched=90", "created=3", "queued=3", "status=partial", "group=R2", "parsed=18", "expected=20"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "zeroed=") {
		t.Error("roster summary should not report a zeroed count — that is VS-only")
	}
}

func TestIngestDispatchesTheVSRouteFromTheCaptureRow(t *testing.T) {
	var out, errOut bytes.Buffer
	calls := &callLog{}
	svc := fakeIngester{
		route: "vs_ranking", status: "complete",
		matched: 94, queued: 2, zeroed: 2,
		calls: calls,
	}
	code := runIngest(&out, &errOut, []string{"--capture", "9"}, svc)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if calls.vsCalls != 1 || calls.rosterCalls != 0 {
		t.Fatalf("calls = %+v, want exactly one IngestVS call and zero IngestRoster calls", calls)
	}
	got := out.String()
	for _, want := range []string{"route=vs_ranking", "matched=94", "queued=2", "zeroed=2", "status=complete"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout %q missing %q", got, want)
		}
	}
}

func TestIngestRejectsAnUnknownRouteWithoutDoingAnyWork(t *testing.T) {
	var out, errOut bytes.Buffer
	calls := &callLog{}
	svc := fakeIngester{route: "fleet", calls: calls}
	code := runIngest(&out, &errOut, []string{"--capture", "3"}, svc)
	if code == 0 {
		t.Fatal("want a non-zero exit for an unknown route")
	}
	if calls.rosterCalls != 0 || calls.vsCalls != 0 {
		t.Fatalf("calls = %+v, want neither route run for an unrecognized capture route", calls)
	}
	if out.Len() != 0 {
		t.Errorf("stdout should stay empty when the route is unrecognized, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "fleet") {
		t.Errorf("errOut = %q, want it to name the unrecognized route", errOut.String())
	}
}

func TestIngestDefaultPeriodKeyForVSIsTheISOWeekOfTheCaptureNotNow(t *testing.T) {
	var out, errOut bytes.Buffer
	calls := &callLog{}
	// A fixed, long-past timestamp: if the default period came from
	// wall-clock now instead of the capture, this would fail on every day
	// except the one the test happened to run on.
	svc := fakeIngester{
		route:     "vs_ranking",
		startedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		calls:     calls,
	}
	code := runIngest(&out, &errOut, []string{"--capture", "9"}, svc)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if calls.vsPeriod != "2026-W33" {
		t.Errorf("period passed to IngestVS = %q, want 2026-W33", calls.vsPeriod)
	}
	if !strings.Contains(out.String(), "period=2026-W33") {
		t.Errorf("stdout %q missing period=2026-W33", out.String())
	}
}

func TestIngestDefaultPeriodKeyForRosterIsTheDateOfTheCaptureNotNow(t *testing.T) {
	var out, errOut bytes.Buffer
	calls := &callLog{}
	// Same reasoning as the VS test above, for the roster route's date-form
	// default: a fixed, long-past timestamp so a wall-clock fallback would
	// fail on every day except the one this test happened to run on.
	svc := fakeIngester{
		route:     "roster",
		startedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		calls:     calls,
	}
	code := runIngest(&out, &errOut, []string{"--capture", "9"}, svc)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if calls.rosterPeriod != "2026-08-12" {
		t.Errorf("period passed to IngestRoster = %q, want 2026-08-12", calls.rosterPeriod)
	}
	if !strings.Contains(out.String(), "period=2026-08-12") {
		t.Errorf("stdout %q missing period=2026-08-12", out.String())
	}
}

func TestIngestExplicitPeriodFlagOverridesTheDefault(t *testing.T) {
	var out, errOut bytes.Buffer
	calls := &callLog{}
	svc := fakeIngester{
		route:     "vs_ranking",
		startedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		calls:     calls,
	}
	code := runIngest(&out, &errOut, []string{"--capture", "9", "--period", "2026-W01"}, svc)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, errOut.String())
	}
	if calls.vsPeriod != "2026-W01" {
		t.Errorf("period passed to IngestVS = %q, want the explicit override 2026-W01", calls.vsPeriod)
	}
}

func TestIngestReportsALoadCaptureFailureWithoutRunningEitherRoute(t *testing.T) {
	var out, errOut bytes.Buffer
	calls := &callLog{}
	svc := fakeIngester{captureErr: errors.New("boom"), calls: calls}
	code := runIngest(&out, &errOut, []string{"--capture", "9"}, svc)
	if code == 0 {
		t.Fatal("want a non-zero exit when the capture row cannot be loaded")
	}
	if calls.rosterCalls != 0 || calls.vsCalls != 0 {
		t.Fatalf("calls = %+v, want neither route run when the capture lookup fails", calls)
	}
}

func TestIngestLogsAndResultsGoToDifferentWriters(t *testing.T) {
	var out, errOut bytes.Buffer
	svc := fakeIngester{route: "roster", rosterErr: errors.New("ocr backend unavailable")}
	code := runIngest(&out, &errOut, []string{"--capture", "9"}, svc)
	if code == 0 {
		t.Fatal("want a non-zero exit when the route fails")
	}
	if out.Len() != 0 {
		t.Errorf("stdout should stay empty on a failed run, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "level=ERROR") {
		t.Errorf("errOut = %q, want the failure logged there", errOut.String())
	}
	if !strings.Contains(errOut.String(), "ocr backend unavailable") {
		t.Errorf("errOut = %q, want the underlying error in the log line", errOut.String())
	}
}
