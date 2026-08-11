package tasks

import (
	"os"
	"testing"
	"time"
)

// TestMain collapses the humanization pacing for the device-free suite.
//
// The settle durations exist so the tap stream is not a metronome, which is a
// property of the real device and not of these tests: tapUntilGone's cap test
// alone would otherwise sit through maxDonations settles doing nothing. The
// suite must pass with no emulator, no adb, and no Docker, and it should do it
// fast enough that nobody is tempted to skip it.
func TestMain(m *testing.M) {
	tapSettle = time.Millisecond
	tabSettle = time.Millisecond
	claimPollInterval = time.Millisecond
	claimPollBudget = 2 * time.Second
	executeSettle = time.Millisecond
	os.Exit(m.Run())
}
