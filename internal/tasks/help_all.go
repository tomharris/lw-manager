package tasks

// help_all taps the help icon that floats on the base HUD. It is not on the
// alliance screen — an earlier skeleton assumed it was, and the audit found
// otherwise, which is why this task navigates nowhere.
//
// The icon is absent when nobody needs help, and that is success rather
// than failure: at a 180s cadence it is the common case.
func init() { Register("help_all", baseTapTask("help_all_button")) }
