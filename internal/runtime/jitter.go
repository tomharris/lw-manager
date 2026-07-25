package runtime

import (
	"math/rand"
	"time"
)

// Jitter draws a uniform duration in [min, max]. Fixed timing is the most
// detectable signal the platform emits, so every wait funnels through here
// (invariant #7). It is exported so callers outside the task runtime — the
// corpus recorder, for one — can comply without constructing a Ctx.
// Inverted or equal bounds collapse to min rather than panicking: a
// misconfigured range should still humanize, not crash mid-task.
func Jitter(r *rand.Rand, min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(r.Int63n(int64(max-min)+1))
}
