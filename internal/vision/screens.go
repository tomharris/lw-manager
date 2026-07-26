package vision

// ScreenNames is the recognizable game screen set: every screen a corpus frame
// may be labeled with, and therefore every screen the recognizer must be able
// to name.
//
// This is the single declaration of that vocabulary. It lives here, in the
// package that does the recognizing, because both consumers can reach it:
// internal/studio (which offers the labels and validates crops against them)
// already imports vision, and the M1 gate is a vision test. Declaring it in
// studio instead would put it out of the gate's reach — studio imports vision,
// so vision cannot import studio back — and the two copies would drift. That
// drift is silent in the worst way: add a screen to the labelling UI, forget
// the gate, and the gate simply stops checking the screen you just added.
//
// A screen belongs here when the recognizer must name it, which is not the
// same as the graph navigating to it. alliance_members and vs_ranking have no
// DefaultGraph edges and are still listed: they are labeled in the corpus for
// M4, and a labeled screen with no identifying anchor is scored wrong on every
// run, forever. Recognition and navigation are separate concerns.
//
// Mail is a tree rather than a tabbed screen. `mail` is the mailbox index
// reached from base, and each mailbox drills down into its own screen carrying
// the message list and the claim-all button. They are separate screens because
// a task must know which mailbox it is standing in before it claims anything —
// recognizing "some mail screen" and tapping claim-all is the blind tap
// invariant #3 forbids. Only the three mailboxes we act on are modelled.
var ScreenNames = []string{
	"base",
	"world_map",
	"alliance",
	"alliance_tech",
	"alliance_members",
	"vs_ranking",
	"mail",
	"mail_alliance",
	"mail_event",
	"mail_system",
	"radar",
}
