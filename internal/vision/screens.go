package vision

// Screen names, as constants rather than bare strings. The screen graph,
// every task body, and the synthetic test registry all name these; a typo
// in a string literal compiles and fails at runtime, where it presents as a
// mysterious "unknown screen" mid-task.
const (
	ScreenBase               = "base"
	ScreenWorldMap           = "world_map"
	ScreenAlliance           = "alliance"
	ScreenAllianceTech       = "alliance_tech"
	ScreenAllianceTechDonate = "alliance_tech_donate"
	ScreenAllianceMembers    = "alliance_members"
	ScreenAllianceDuel       = "alliance_duel"
	ScreenVS                 = "vs"
	ScreenVSRanking          = "vs_ranking"
	ScreenVSRankingWeekly    = "vs_ranking_weekly"
	ScreenMail               = "mail"
	ScreenMailAlliance       = "mail_alliance"
	ScreenMailEvent          = "mail_event"
	ScreenMailSystem         = "mail_system"
	ScreenRadar              = "radar"
	ScreenStaminaPrompt      = "stamina_prompt"
)

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
// same as the graph navigating to it. stamina_prompt and alliance_tech_donate
// appear in DefaultGraph with out-edges only — entered by tapping something
// whose effect the graph cannot predict, never routed to — and are listed
// here regardless: a labeled screen with no identifying anchor is scored
// wrong on every run, forever. Recognition and navigation are separate
// concerns.
//
// Two of these are trees rather than single screens, and both are modelled the
// same way: one screen per state a task must act *from*. Invariant #3 requires
// a matched anchor before every tap, so a state you tap from is a state you
// must be able to name. Recognizing "some mail screen" and tapping claim-all,
// or "some ranking screen" and parsing it, is the blind tap that invariant
// forbids — and for the ranking it would feed M4 numbers off whichever view
// happened to be showing, which invariant #4's "every number traces back to a
// screenshot" cannot save you from if the screenshot is the wrong screen.
//
// Mail: `mail` is the mailbox index reached from base, and each mailbox drills
// down into its own screen carrying the message list and the claim-all button.
// Only the three mailboxes we act on are modelled.
//
// VS ranking: base -> alliance_duel -> vs_ranking -> "weekly ranking" tab.
// base's VS button lands on Alliance Duel, not on a ranking screen, so the
// screen the button actually opens gets its own name rather than being
// folded into the tree it merely leads to. It is NOT reachable from Alliance;
// docs that describe the route as "Alliance -> Members -> VS Ranking" are
// wrong.
//
// The filtered and unfiltered weekly views are one screen, vs_ranking_weekly,
// not two. They used to be modelled separately, but they differ only by
// whether a checkbox is ticked, and template matching cannot express the
// absence of something: a crop of the empty checkbox correlates with any
// smooth region, so it is not an identifying anchor, it is a threshold that
// happens to pass. The filter is a state within the screen, confirmed by
// querying for the your_alliance_checked anchor after tapping the filter
// button, not a screen a task navigates to.
//
// alliance_tech_donate is the dialog reached from the recommended tech.
// stamina_prompt is the buy/refill dialog Quick Execute raises when stamina is
// short — named not because a task acts from it, but so a task can recognize
// it and leave. It is the one screen here that spends real currency, and
// nothing on it is ever tapped.
//
// The rewards celebration is deliberately NOT a screen. It is a transient
// animation over a fully visible origin, so an overlay frame is its origin —
// mail_alliance, radar — and is labelled that way. Naming it would demand ten
// corpus frames of a screen that does not exist.
var ScreenNames = []string{
	ScreenBase,
	ScreenWorldMap,
	ScreenAlliance,
	ScreenAllianceTech,
	ScreenAllianceTechDonate,
	ScreenAllianceMembers,
	ScreenAllianceDuel,
	ScreenVS,
	ScreenVSRanking,
	ScreenVSRankingWeekly,
	ScreenMail,
	ScreenMailAlliance,
	ScreenMailEvent,
	ScreenMailSystem,
	ScreenRadar,
	ScreenStaminaPrompt,
}
