package scheduler

import (
	"strconv"
	"time"
)

// maxBackoffFailures caps the exponential backoff so a permanently-broken
// task settles at a long-but-bounded retry interval instead of overflowing.
const maxBackoffFailures = 5

// backoff is the extra delay added to a task's base cadence after a run of
// consecutive failures, so the total spacing from the last attempt is
// cadence × 2^min(failures, maxBackoffFailures). Zero when nothing has
// failed. Failures beyond the cap reuse the cap's value.
func backoff(cadence time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures > maxBackoffFailures {
		failures = maxBackoffFailures
	}
	return cadence * time.Duration((int64(1)<<uint(failures))-1)
}

// cadenceOffset is a derived jitter in [-0.2, +0.2] × cadence. Firing a task
// on exact cadence boundaries is the fixed pattern invariant #7 exists to
// defeat, but Plan must stay pure — so the offset is hashed from stable
// inputs. Including lastRun makes it move run-to-run while staying fixed
// within a single planning tick.
func cadenceOffset(accountID int64, taskName string, lastRun time.Time, cadence time.Duration) time.Duration {
	h := hash64(strconv.FormatInt(accountID, 10), taskName, strconv.FormatInt(lastRun.UnixNano(), 10))
	frac := (float64(h%40001)/40000.0)*0.4 - 0.2 // -0.2 … +0.2
	return time.Duration(frac * float64(cadence))
}
