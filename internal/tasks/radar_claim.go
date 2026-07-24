package tasks

// radar_claim runs Claim All on the radar screen. Claims only award points
// on VS-scoring days, so the tasks table schedules it Mon/Wed/Fri/Sat and at
// most once a day — the body itself needs no calendar logic. Skeleton:
// navigate to radar and tap; the real body performs Claim All.
func init() { Register("radar_claim", collectTask("radar", "radar_claim_button")) }
