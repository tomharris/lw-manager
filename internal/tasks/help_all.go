package tasks

// help_all taps the alliance help-all button. The button is absent when no
// one needs help; that is success, not failure.
func init() { Register("help_all", baseTapTask("collect_bubble")) }
