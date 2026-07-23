package tasks

// daily_gather collects the base's accumulated resources. Skeleton: the
// real task will iterate multiple resource buildings once corpus templates
// name them.
func init() { Register("daily_gather", collectTask("base", "gather_button")) }
