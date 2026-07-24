package runtime

import (
	"math/rand"
	"time"
)

// jitter draws a uniform duration in [min, max]. Fixed timing is the most
// detectable signal the platform emits, so every wait in the runtime funnels
// through here (invariant #7). Inverted or equal bounds collapse to min
// rather than panicking: a misconfigured range should still humanize, not
// crash mid-task.
func jitter(r *rand.Rand, min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(r.Int63n(int64(max-min)+1))
}
