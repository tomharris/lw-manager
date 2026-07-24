package tasks

// radar_quick runs Quick Execute on the radar screen. Worth doing every time
// the radar is seen, so it is scheduled on all days (see the tasks table).
// Skeleton: navigate to radar and tap; the real body performs Quick Execute.
func init() { Register("radar_quick", collectTask("radar", "radar_quick_button")) }
