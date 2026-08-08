package tasks

// daily_gather taps a resource collect bubble on the base. Tapping any one
// bubble collects every building's accumulation, so a single tap is the
// whole task — the earlier skeleton's plan to iterate buildings was wrong
// about the game.
//
// The bubble's anchor is content-addressed with a broad search region: it
// appears above whichever building has accumulated, so it has no fixed
// position. vision.Match returns the best-scoring placement, and since any
// bubble does the job, "best" needs no disambiguation.
//
// The loot truck is collected separately and is deliberately not handled
// here; it gets its own task.
func init() { Register("daily_gather", baseTapTask("collect_bubble")) }
