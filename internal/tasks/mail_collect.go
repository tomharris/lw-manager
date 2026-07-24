package tasks

// mail_collect claims mail rewards via collect-all. Skeleton: the real task
// will need per-tab collection (system/alliance/report) once mapped.
func init() { Register("mail_collect", collectTask("mail", "collect_all_button")) }
