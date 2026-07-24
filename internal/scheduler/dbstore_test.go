package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
)

type fakeSource struct{ snap db.SchedulerSnapshot }

func (f fakeSource) SchedulerSnapshot(ctx context.Context, serials []string) (db.SchedulerSnapshot, error) {
	return f.snap, nil
}

func TestDBStoreMapsRowsToSnapshot(t *testing.T) {
	last := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	src := fakeSource{snap: db.SchedulerSnapshot{
		Paused:   true,
		Accounts: []db.ScheduledAccount{{ID: 3, Role: "farm", Enabled: true}},
		Tasks: []db.ScheduledTask{
			{Name: "radar_claim", CadenceSeconds: 86400, Roles: []string{"farm"}, DaysOfWeek: []int16{1, 3, 5, 6}, Enabled: true},
		},
		Runs: []db.TaskRunState{
			{AccountID: 3, TaskName: "radar_claim", LastRun: &last, ConsecutiveFailures: 2},
			{AccountID: 3, TaskName: "help_all", LastRun: nil, ConsecutiveFailures: 0},
		},
	}}

	got, err := NewDBStore(src).SchedulerSnapshot(context.Background(), []string{"s"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused {
		t.Error("paused not carried through")
	}
	if len(got.Tasks) != 1 || got.Tasks[0].Cadence != 24*time.Hour {
		t.Fatalf("task cadence mapping: %+v", got.Tasks)
	}
	if len(got.Tasks[0].Days) != 4 || got.Tasks[0].Days[1] != time.Wednesday {
		t.Fatalf("weekday mapping: %+v", got.Tasks[0].Days)
	}
	rs := got.Runs[RunKey{3, "radar_claim"}]
	if !rs.HasRun || !rs.LastRun.Equal(last) || rs.ConsecutiveFailures != 2 {
		t.Fatalf("run state mapping: %+v", rs)
	}
	// A nil LastRun must map to HasRun=false.
	if got.Runs[RunKey{3, "help_all"}].HasRun {
		t.Error("nil last_run should map to HasRun=false")
	}
}
