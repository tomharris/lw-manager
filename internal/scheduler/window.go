// Package scheduler decides which task to run for which account, and when.
//
// The policy lives in Plan, a pure function over a Snapshot. All randomness
// (cadence jitter, offline window) is derived by hashing stable inputs
// rather than drawn from a RNG: that keeps Plan pure and testable, keeps a
// process restart from re-rolling a decision, and still drifts across days
// because the date is one of the hashed inputs.
package scheduler

import (
	"hash/fnv"
	"strconv"
	"time"
)

// hash64 is a stable FNV-1a hash of its parts, NUL-separated so ("a","bc")
// and ("ab","c") differ.
func hash64(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// OfflineWindow returns the account's do-not-run window for the given date's
// day, computed in that date's location. The platform must not run a device
// 24/7 (detection avoidance): each account gets a 5–7h daily offline window
// whose start and length drift daily. Derived, not stored, so a restart
// recomputes the identical window instead of granting fresh eligibility.
func OfflineWindow(accountID int64, date time.Time) (start, end time.Time) {
	y, m, d := date.Date()
	loc := date.Location()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, loc)

	h := hash64(strconv.FormatInt(accountID, 10), date.Format("2006-01-02"))
	startMin := int(h % (24 * 60))    // any minute of the day
	durMin := 300 + int((h/1440)%121) // 300..420 minutes = 5h..7h

	start = midnight.Add(time.Duration(startMin) * time.Minute)
	end = start.Add(time.Duration(durMin) * time.Minute)
	return start, end
}

// inOfflineWindow reports whether now falls in the account's offline window.
// It checks today's window and yesterday's, because a window that starts
// late (e.g. 22:00) runs past midnight into the following morning.
func inOfflineWindow(accountID int64, now time.Time) bool {
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		start, end := OfflineWindow(accountID, day)
		if !now.Before(start) && now.Before(end) {
			return true
		}
	}
	return false
}
