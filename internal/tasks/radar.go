package tasks

// radar claims completed radar missions. Skeleton: the real task will need
// per-mission claims and a completion sweep once the screen is mapped.
func init() { Register("radar", collectTask("radar", "radar_claim_button")) }
