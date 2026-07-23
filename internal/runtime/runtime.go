// Package runtime executes tasks against a device through a constrained set
// of primitives.
//
// The primitives are the enforcement point for three platform invariants:
// every primitive checks the kill switch before acting (#8), Tap accepts
// anchors rather than coordinates so no blind tap can be expressed (#3), and
// Sleep is jittered by construction (#7). Task code gets no access to the
// underlying Transport, so none of these can be bypassed.
package runtime

import "errors"

var (
	// ErrPaused reports the global kill switch (flags.pause_all) is set.
	ErrPaused = errors.New("runtime: fleet is paused")
	// ErrAccountDisabled reports the per-account kill switch.
	ErrAccountDisabled = errors.New("runtime: account is disabled")
	// ErrLost reports that no screen could be recognized even after the
	// panic route (back ×3, then app restart) ran. The task run is failed
	// and the agent stops rather than flails.
	ErrLost = errors.New("runtime: lost - no screen recognized after recovery")
	// ErrWrongScreen reports a recognized screen other than the expected one.
	ErrWrongScreen = errors.New("runtime: not on the expected screen")
	// ErrAnchorNotFound reports an anchor that failed to match on an
	// otherwise-recognized screen. Tasks may treat this as "nothing to do"
	// (a help-all button absent means no help requests).
	ErrAnchorNotFound = errors.New("runtime: anchor not found on screen")
)
