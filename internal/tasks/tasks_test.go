package tasks

import (
	"reflect"
	"testing"
)

// The registry is the contract the tasks table in the database is written
// against: a row naming a task the registry does not know fails the agent at
// startup. radar_quick and radar_claim are gone, merged into one radar task —
// see migration 00004 and radar.go for why the split could not work.
//
// The behaviour the deleted skeleton tests covered — that each task runs
// against synthetic screens and tolerates a missing anchor — is now covered
// per task, against explicit present-and-absent frames, rather than by a
// shared script that could only assert "some tap happened".
func TestOnlyTheMergedRadarTaskIsRegistered(t *testing.T) {
	want := []string{"daily_gather", "help_all", "mail_collect", "radar", "tech_donate"}
	if got := Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names: got %v, want %v", got, want)
	}
}
