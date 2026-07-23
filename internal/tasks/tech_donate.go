package tasks

// tech_donate makes one alliance tech donation. Skeleton: the real task
// will tap repeatedly until the daily cap and read the donation counter.
func init() { Register("tech_donate", collectTask("alliance_tech", "donate_button")) }
