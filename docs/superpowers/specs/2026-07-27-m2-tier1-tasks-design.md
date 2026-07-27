# M2 — Tier 1 task bodies and the unattended run

**Date:** 2026-07-27
**Status:** design agreed, awaiting implementation plan
**Predecessor:** `2026-07-25-corpus-tooling-m1-gate-design.md` (M1 closed at 99.72%)

---

## 1. Context

M1 proved the agent can know where it is. Nothing yet proves it can do
anything.

`templates/manifest.yaml` carries 41 anchors and every one of them is
navigational or identifying. Not one action anchor exists, so the six Tier 1
tasks in `internal/tasks/` are one-line skeletons naming templates that were
never cut:

```go
// internal/tasks/help_all.go:5
func init() { Register("help_all", collectTask("alliance", "help_all_button")) }
```

`Graph.Validate` does not catch this — it checks only the edges the graph
names — so these fail at `Tap` time with `screen %q has no anchor %q`. The
skeletons compile, register, and pass their tests because the tests use a
synthetic registry (`internal/runtime/runtimetest/runtimetest.go:44`) that
declares the missing anchors.

This milestone cuts the real anchors, writes the real bodies, and runs one
account unattended for 24 hours.

**In scope:** action anchors, two new screens, the graph edges to reach them,
a shared-template region guard in the registry, five task bodies, a migration
reshaping the tasks table, and a two-phase acceptance gate.

**Out of scope:** the loot truck (its own follow-up task), M3 fleet work, and
all M4 analytics collection. `alliance_members` and the `vs` tree stay
unrouted in the graph — they are recognized for M4's benefit and navigating
them is M4's problem.

---

## 2. The flow audit

The six flows were guessed from skeleton comments and then audited against the
game. Every one of the six differed materially from the guess — two of them in
ways that would have shipped a task reporting success for doing nothing.
Recording what was learned matters
more than recording the conclusions, because the same misjudgements are
available to anyone who plans the next milestone from the same documents.

Dimensions that change a design rather than a crop:

| | dimension | cost when true |
|---|---|---|
| D1 | screen has sub-screens | new `ScreenNames` entry, ≥10 corpus frames, anchor, edges |
| D2 | tap raises a popup | same, plus escape edges |
| D3 | action repeats | bounded loop |
| D4 | how the task knows it is done | may force OCR instead of anchor absence |
| D5 | "nothing to do" ⇒ button vanishes or greys | **determines whether success is real** |
| D6 | target needs scrolling into view | scroll-to-find is unbuilt |
| D7 | animation outlasts `WaitFor`'s 20s | silent gate failure |

### Findings

| task | guessed | actual |
|---|---|---|
| `help_all` | on `alliance`, tap the help button | **floating icon on `base`** — no navigation at all |
| `daily_gather` | iterate resource buildings | **any one collect bubble collects everything** |
| `mail_collect` | 3 mailboxes, Claim All each | correct — but Claim All is **always present and always enabled** |
| `tech_donate` | tap a tech, donate N times | correct that it is a tree; the tech is chosen by a **red thumbs-up badge**, the screen has **two tabs** and the recommendation may be on either, and the donate button **greys rather than vanishing** |
| `radar_quick` / `radar_claim` | two independent tasks | **one flow** — the buttons are mutually exclusive and a pending claim blocks the next execution |

### D5 is where a task can pass the gate while doing nothing

`collectTask` treats `ErrAnchorNotFound` as "nothing to collect, success". That
is sound only when the button is *absent*. A greyed-out button is still
rendered, and NCC on a desaturated-but-identically-shaped button will usually
clear 0.85: the anchor matches, `Tap` fires, the game ignores it, and `Run()`
records `succeeded`. Six tasks doing that for 24 hours reads as a clean pass.

This is the `vs_ranking_weekly` empty-checkbox lesson from `CLAUDE.md` one
level up — again asking a correlation score to confirm that something is *not*
there. The fix is the same shape: find a positive discriminator, never a looser
threshold.

Two tasks hit D5 and each resolves differently:

- **`tech_donate`** — a counter or label changes alongside the colour. Crop the
  donate anchor to *include* that counter, so the enabled state is identified
  by something present. A structural discriminator beats a chromatic one here
  because NCC runs on intensity, and a pure desaturation can leave the
  correlation nearly untouched.
- **`mail_collect`** — Claim All never changes state, so absence carries no
  information at all. Tapping when there is nothing to claim is a harmless
  no-op. The task therefore cannot detect "nothing to claim" and must not try;
  what it must handle is that **the rewards popup is optional.**

### The recommendation hides behind a tab, and the signal self-suppresses

`alliance_tech` has two tabs and the recommended tech may be on either. The tab
holding it carries the same badge on its header — **but only while that tab is
unselected.** Selecting the tab removes the badge from it. Both badges are
tappable, and the tab's badge switches tabs.

With tab A selected, the state space is:

| recommendation is | tab badge | tech badge in list |
|---|---|---|
| on A (selected) | **hidden** | present |
| on B (unselected) | present on B's header | absent |
| nowhere | absent | absent |

So *"no tab badge"* is ambiguous — the recommendation is either on this tab or
does not exist. The signal is suppressed exactly when it would disambiguate.
That is the presence/absence problem for the fourth time in this milestone.

It resolves **without ever testing for absence**, by ordering two presence
checks (§6). Each step asks "is this here?", which NCC can answer; nothing asks
"is this missing?", which it cannot. Where a previous section had to find a
different discriminator, here the ambiguity dissolves purely in the order the
questions are asked — worth noting as the cheapest resolution of this pattern
found so far.

Each tab fits on one screen: the recommendation is never below the fold, so no
scroll-and-search is needed (D6 clear).

### There is no popup, and no scrim — corrected by measurement

An earlier draft of this section argued at length that the rewards overlay is a
centred panel over a dimmed origin, that both screens therefore qualify in the
recognizer, and that a `modal` precedence rule was needed to break the tie. The
reconnaissance capture (`2026-07-27-m2-recon-findings.md`) measured it, and the
premise was false.

**The rewards display is a transient celebration animation, not a dialog.**

| assumed | measured |
|---|---|
| dimmed scrim over the origin | **no dimming** — background at full brightness |
| a close/X button | **none anywhere** |
| origin partly obscured | origin **fully visible and identifiable** |
| dismissed by tapping close | **dismissed by tapping anywhere** |

The argument rested on the algebra of NCC under a scrim: `p → αp + (1−α)c`
leaves the numerator and denominator both scaled by `α`, so the score is
mathematically unchanged. That derivation is correct. **It is also about a
world this game does not implement** — there is no scrim to be invisible to.

That is why §8 lists measurement of a derived claim as an audit item rather
than treating the derivation as settled. **A derivation can be sound and still
be about the wrong thing**, and the only defence is to look.

The consequence is a simplification: an overlay frame is not "the popup over
the mail screen", it *is* the mail screen with an animation playing on it. The
recognizer is already correct about it and needs no change. Deleted from this
design: the `rewards_popup` screen, the `modal` manifest field and precedence
rule, the `close_button` anchor, the popup's four escape edges, and
`dismissModal`.

### Dismissing the animation is still an action

Tap-anywhere is not free. Invariant #3 forbids acting without a matched anchor,
and "anywhere" is not a coordinate this design is allowed to express.

The dismiss target is the **`CONGRATULATIONS!` banner itself**. Tapping it is
tapping "anywhere", and the anchor is self-gating: it matches only while the
animation plays, so when there is nothing to dismiss `tapIfPresent` returns
false and no tap happens at all.

The rejected alternative is worth recording. The bottom-left corner of both the
radar and every mailbox holds a **back arrow**, and tapping there does dismiss
the animation — the catcher swallows the tap and the arrow never fires. But if
the animation has already cleared, the same tap hits a real Back button:
harmless in a mailbox, and on the radar an exit to `base` mid-task. The banner
removes that failure mode instead of timing around it. **Prefer the anchor
whose presence is the condition you are testing for.**

### Ambient toasts occlude the top of any screen

The recon caught a *"Sarah's Gift received!"* notification sliding across the
top of the radar, unrelated to anything the agent did. These arrive
spontaneously and will land in corpus frames.

This is the `world_map` return-home bubble from M1 in a new guise, and it cost
three recrops there before the cause was found. **Prefer anchors from the
bottom bar**; treat any top-of-screen crop as occlusion-prone and check its
failing frames before its threshold.

### The radar contradiction

Two facts learned in the audit are jointly decisive:

1. Quick Execute and Claim All are never on screen together.
2. A pending Claim All blocks the next Quick Execute.

The tasks table (`00003_tasks.sql:23`) gates `radar_claim` to `{1,3,5,6}`
because claims only score VS points on those days. Under (1) and (2) that
banking blocks every execution in between, so `radar_quick` at its 10800s
cadence would find no button ~8 times a day and record ~8 successes for doing
nothing. The schedule was written against a model of the game that does not
hold.

**Resolution:** one `radar` task, scheduled `{1,3,5,6}`. Targets accumulate
untouched on non-scoring days, which preserves the VS optimization the original
schedule was reaching for, and immediate claiming makes the blocking constraint
harmless.

### Quick Execute lies, and tapping it can spend money

The recon added a constraint nothing in this design anticipated. One frame
reads:

> **"You can complete 6 tasks, requires 60 Stamina"** — with **⚡31** in the HUD,
> and Quick Execute **present and rendered enabled**.

So the button's presence does not mean the action can succeed, and **tapping it
with insufficient stamina opens a buy/refill prompt** — a dialog that spends
currency, now reachable by an automated tap. A second HUD element reads
`29/40` beside *"13 Radar Task(s) will be restored in 05:14:33"*: a capped pool
that regenerates over time, on top of the per-task stamina cost.

This kills the sweep loop. A loop terminating when Quick Execute goes *absent*
never terminates on insufficient stamina, because the button is never absent
merely because you are broke — it would run to `maxRadarSweeps` and report
`ErrTapCapReached`, a defect verdict, on a radar that was simply out of
stamina.

It is also the disabled-control trap in its most expensive form yet. The
donate button greys; mail's Claim All no-ops; **Quick Execute charges you.**

**Resolution:** a single execute-then-claim pass per run, and a named
`stamina_prompt` screen the task recognizes and leaves without touching
anything. Stamina regenerates on its own, so the next run three hours later
can afford what this one could not — being out of stamina is an *ordinary
outcome*, not a failure. Reading the stamina figure would need OCR, which this
milestone deliberately excludes; recognition is what protects us instead, which
is invariant #3 earning its keep.

The earlier concern about targets accumulating is resolved: one frame shows
**"11 task rewards can be claimed"**, so rewards bank in quantity — an open audit
item (§8).

---

## 3. Vocabulary: +2 screens, and the M1 gate re-runs

`internal/vision/screens.go` gains:

- **`alliance_tech_donate`** — the donate dialog reached from the recommended
  tech.
- **`stamina_prompt`** — the buy/refill dialog Quick Execute raises when
  stamina is short (§2). It is modelled for one reason only: so the agent can
  *recognize* it and leave. A screen that spends currency is the strongest
  possible case for invariant #3 — the task must never act on it beyond
  escaping, and it can only refuse to act on a screen it can name.

**`rewards_popup` is not a screen** and was removed after measurement (§2). The
celebration animation plays over its origin, which stays fully recognizable, so
an overlay frame is labelled as its origin — `mail_alliance`, `radar` — which
is what it is.

`internal/vision/corpus_test.go:77` requires **≥10 corpus frames for every name
in `ScreenNames`**, and the gate demands ≥98% across all of them.

> **This milestone does not close until `make gate` passes again at 16
> screens.** The M1 gate is not a one-time event; it is a standing invariant
> that every vocabulary change re-tests.

### A screen we name is a screen we own

Today an unrecognized dialog is handled incidentally: `recognizeOrRecover` →
`panicRoute` → back ×3 (`panic.go:12`, *"Popups and interstitials die to
back"*). Once `stamina_prompt` is in `ScreenNames` with an anchor, it
recognizes cleanly, the panic route never fires for it, and `NavigateTo` calls
`Path(stamina_prompt, target)` — which returns `ErrNoPath` for a node with no
edges, failing the task.

Naming a screen transfers it from the panic route's care to the graph's, and
**the graph must be updated in the same change**. `stamina_prompt` therefore
ships with its escape edge or not at all.

This cuts both ways, and it is why the vocabulary is not a free-for-all. Left
unnamed, the stamina dialog would have been dismissed by the panic route with
three back presses — accidentally correct, but as a side effect of *failing to
recognize* it, and a run whose recovery path fires routinely has no signal left
for real trouble.

---

## 4. Anchors

| anchor | screen | note |
|---|---|---|
| `help_all_button` | `base` | content-addressed, broad search region |
| `collect_bubble` | `base` | same; must be separable from `help_all_button` |
| `tech_recommended_badge` | `alliance_tech` | region over the tech list; see below |
| `tab_recommended_badge` | `alliance_tech` | **same template**, region over the tab headers |
| identifying anchor | `alliance_tech_donate` | |
| `donate_button` | `alliance_tech_donate` | crop **includes the counter** (D5) |
| `quick_execute_button` | `radar` | presence *is* the state |
| `claim_all_button` | `radar` | presence *is* the state |
| `claim_all_button` | `mail_event`, `mail_system` | `mail_alliance` already has one (`manifest.yaml:159`) |
| `rewards_banner` | `radar`, `mail_alliance`, `mail_event`, `mail_system` | the `CONGRATULATIONS!` banner; **self-gating dismiss target** (§2) |
| identifying anchor | `stamina_prompt` | recognition only — nothing on this screen is ever tapped |

`rewards_banner` is likely one template across all four screens, but that is
**unconfirmed**: the recon captured the celebration in a mailbox and never on
the radar. If the radar's differs, it is a second template, not a looser
threshold. It should also be a strong anchor on its own merits — a large,
high-contrast orange banner with white text, at the opposite end of the
variance scale from the empty checkbox that started this discussion.

`stamina_prompt` gets an identifying anchor and nothing else. It has no action
anchor because it has no action: the only correct interaction is to leave.

Three of these are unlike anything currently in the manifest.

**Broad-region content anchors.** Every existing anchor is a tight crop in a
small `Region` because the button's location is known. The collect bubble
appears above whichever building has accumulated, and the help icon floats on
the HUD; both need regions covering most of the screen. `vision.Match` returns
the best-scoring match, and since any bubble collects everything, "best"
needing no disambiguation is exactly the property the task wants.

`base` carries 59 corpus frames — the most of any label — so there is material
to measure against. Both anchors must be checked for separation **against each
other**, not only against other screens: two HUD badges that both mean "tap me"
are precisely the pair a min-aggregated recognizer would let blur.

**The badge is tapped *through*, and that works.** `Tap` jitters inside the
matched box (`act.go:73`), so anchoring on the thumbs-up aims at the badge's
few dozen pixels rather than the tech button's. Confirmed in the audit: the
badge is inside the button's hit area, so tapping it activates the button and
no offset is required.

**The badge template must be exactly the badge.** The tech buttons are uniform
in size with only their contents differing, so the badge is the *only* pixel
content invariant across recommendations. Cropping wider to include the tech's
icon, name, or cost means that when a different tech is recommended that region
differs and the NCC score collapses — widening does not trade tap accuracy for
match reliability here, it destroys the match outright.

**Which removes the usual remedy for a flat crop.** The badge is small, and
`CLAUDE.md` records that a nearly-flat template correlates ~1.0 with any
similarly flat region. Measure its standard deviation against the reference
band (empty checkbox 2,346 / checkbox+check 10,324 / wordmark 25,927) before
trusting it — but note that the normal fix, cropping wider for more structure,
is unavailable in the direction of the button's contents.

If the badge alone proves too flat, widen toward the button's **chrome** — its
border, corner, or panel edge — which is uniform across techs precisely because
the buttons are. The rule is directional: **widen toward what is invariant,
never toward what varies.** A wider crop that reaches the tech icon is worse
than a flat one, because it fails on the techs it was not cut from.

**One template, two anchors, disambiguated only by region.** The tab badge and
the tech badge are the same icon, so a single PNG is referenced by two anchor
entries whose `region` values differ — one over the tab headers, one over the
tech list. `region` is already per-anchor (`manifest.yaml:5–22`), so this needs
no new mechanism.

The load-bearing property is that **the two regions must be disjoint.** If they
overlap, each anchor can match the other's badge and the tab logic in §6
inverts: the task would "find" a tech badge that is really the tab badge, tap
it, switch tabs, and report a donation it never made. Registry validation
should reject overlapping regions for anchors sharing a template on the same
screen, rather than leaving it to whoever draws the crops.

### Offsets from a match: the rule, stated correctly

An earlier draft of this spec said *"never an offset from a match — an offset
from a match is a blind tap wearing a disguise."* That is wrong, and it is
worth correcting here rather than leaving a rule that would misdirect the next
anchor to hit this.

> **Never a coordinate that survives its anchor being wrong.**

That is the property invariant #3 actually protects. A fixed screen coordinate
is dangerous because it still produces a tap after the UI has moved out from
under it. A coordinate *derived from a verified match* inherits the anchor's
failure mode: if the layout shifts, the anchor stops matching, `Tap` returns
`ErrAnchorNotFound`, and no tap happens. That is the guarantee we want, and the
absolutist phrasing discarded it along with the genuinely unsafe case.

**No offset is needed in this milestone** — the badge is tappable, so this
stays a correction to the reasoning rather than a feature. But the case that
forced it is worth recording, because it exposed a conflation that is still in
the code:

`MatchResult.Box` (`matcher.go:17`) does two jobs. It is where the template was
*found*, and `act.go:73` also treats it as where to *aim*. Those coincide for
every anchor cut so far — a wordmark, a nav button — because their
discriminating pixels and their hit area are the same rectangle. Nothing
guarantees that: `Match` wants the box that is maximally *invariant*, `Tap`
wants the box that is maximally aligned with the *touch target*.

When an anchor does force them apart, the answer is an optional `tap_box` on
the anchor, expressed in multiples of the matched box (`[0,0,1,1]` being the
identity and the default, so existing anchors are untouched), declared in
`manifest.yaml` and authored visually in `agent studio`. Match-box multiples
rather than screen fractions, because that states a fact about the UI's own
geometry and scales with whatever the anchor scales with. Deliberately **not
built now** — YAGNI, and an unused offset field is one more thing to tune
wrongly.

---

## 5. Graph

The three mailbox tap anchors **already exist** (`manifest.yaml:130–155`); only
edges are missing.

```
mail                        → mail_alliance | mail_event | mail_system   (tap)
mail_alliance|event|system  → mail                                       (Back)
alliance_tech_donate        → alliance_tech                              (Back)
stamina_prompt              → radar                                      (Back)
```

**No recognizer change.** An earlier draft added a `modal: true` manifest field
and a precedence rule so a popup could outrank the screen it covered. §2's
measurement removed the need: there is no overlay screen competing with its
origin, so `recognize` (`recognizer.go:60`) stays exactly as the M1 gate tuned
it. Recorded because the idea is tempting and someone will re-propose it —
revisit it only when a real full-screen dialog leaves its origin recognizable,
which no screen in this milestone does.

### Conditional nodes get out-edges only, never in-edges

Both `alliance_tech_donate` and `stamina_prompt` are entered by tapping
something whose effect depends on state the graph cannot see. Neither gets an
in-edge:

> **A node whose entry depends on transient game state gets out-edges only.**
> The graph models routes that are always available; conditional transitions
> are task logic.

Two distinct failures motivate it, and they are worth separating.

**For `alliance_tech_donate`, an in-edge is a trap.** The edge would name
`tech_recommended_badge`, which is absent whenever the recommendation sits on
the unselected tab (§2). `NavigateTo("alliance_tech_donate")` would then fail
with `ErrAnchorNotFound` — and `walk` only re-plans on `ErrWrongScreen`
(`navigate.go:36`), so the error propagates and the task fails, when in fact
the dialog is one tab-tap away. Leaving the edge out means the only way in is
the task's own tab logic, which is the code that knows how to look.

**For `stamina_prompt`, an in-edge would be an instruction to spend money.**
The edge would name `quick_execute_button` on `radar` — a button that usually
starts an execution and *sometimes* opens a purchase dialog. Modelling that as
a navigable route would make "get to the stamina prompt" a thing `NavigateTo`
could be asked to do, and a re-plan could then choose it as a path segment. The
graph must not contain a route whose traversal is a purchase.

Tab switching is not an edge at all: it is `alliance_tech → alliance_tech`,
intra-screen. Both tabs remain one screen probed by anchors, consistent with
the radar decision (§2).

---

## 6. Task bodies — six become five

No new `Ctx` primitives. Everything composes from `NavigateTo`, `Tap`,
`WaitFor`, `CurrentScreen`, and `Sleep`.

| task | body |
|---|---|
| `help_all` | on `base`, tap `help_all_button`; absent ⇒ success. No navigation. |
| `daily_gather` | on `base`, tap `collect_bubble`; absent ⇒ success |
| `mail_collect` | for each of the three mailboxes: enter, tap Claim All, dismiss the banner if it appeared, back |
| `tech_donate` | `→ alliance_tech`, find the recommendation across both tabs (below), donate until the counter-bearing anchor stops matching, hard-bounded |
| `radar` | **new**, replaces `radar_quick` + `radar_claim`: a single execute-then-claim pass (below) |

`help_all` at a 180s cadence is roughly 320 of the day's ~340 runs and now
never leaves `base` — the dominant contributor to the 24h window is also the
cheapest and least failure-prone thing in it.

### Finding the recommendation across two tabs

`tech_donate` locates the recommendation with two ordered presence checks and
never a test for absence (§2):

1. `Tap(alliance_tech, tech_recommended_badge)` — found ⇒ the donate dialog
   opens. Proceed.
2. Else `Tap(alliance_tech, tab_recommended_badge)` — found ⇒ the tab switches.
   Retry step 1 **once**.
3. Else no recommendation exists ⇒ return nil. Nothing to donate is success,
   the same contract `collectTask` already uses.

The retry is bounded at one because there are exactly two tabs: after switching
once, either the badge is in the list or the game contradicted itself. An
unbounded loop here would ping-pong between tabs forever on a UI change, and it
would do so while reporting nothing wrong.

Step order matters for cost, not correctness. List-first is one step in the
common case where the recommendation is already on the selected tab;
tab-first is two steps in *every* case.

### The radar's single pass

```
NavigateTo(radar)
claim any banked rewards           → a pending claim blocks the next execution
tap Quick Execute                  → absent ⇒ nothing to execute, return nil
if stamina_prompt appeared         → Back out, return nil     (ordinary outcome)
poll for Claim All, tap it, dismiss the banner
```

**Claim-first is load-bearing.** A pending Claim All blocks Quick Execute, so a
run that skipped claiming would find Quick Execute absent, correctly conclude
"nothing to execute", and stop — while sitting on banked rewards it could have
claimed and then executed. Claiming first turns a wasted run into a productive
one.

**No loop.** The sweep loop died with the stamina finding (§2): a loop
terminating on Quick Execute's absence never terminates when the button is
present but unaffordable. One pass per run cannot spiral, and the next run is
three hours away.

**`ErrClaimNeverAppeared` is a real signal again.** It was going to be
swallowed by the stamina case; now that insufficient stamina exits via the
prompt, a Claim All that never appears after a genuine execution means
something is actually wrong.

### Two helpers in `internal/tasks`

**`dismissRewards`** — tap `rewards_banner` if it is present, do nothing if it
is not.

The banner is a **self-gating** dismiss target: it exists only while the
celebration is playing, and tapping it is tapping "anywhere". So the whole
helper is one `tapIfPresent` call, and the "nothing to dismiss" case costs one
match and no tap.

The rejected alternative — tapping the bottom-left corner, which on every one
of these screens is a **back arrow** — works while the animation plays, because
the tap-catcher swallows it. But if the animation has already cleared, the same
tap navigates: harmless in a mailbox, an exit to `base` mid-task on the radar.
**Prefer the anchor whose presence is the condition you are testing for.**

An earlier draft used `WaitFor` here. That is wrong for a screen that
legitimately may not appear: it polls to a 20s deadline and then runs the panic
route (`ctx.go:169`), costing 20s and a spurious back ×3 on every empty
mailbox. With a self-gating anchor the question disappears — there is no screen
to wait for.

**`tapUntilGone`** — `Tap` in a loop, terminating on `ErrAnchorNotFound`, with
a hard iteration cap passed by the caller.

`act.go:47` re-verifies the screen and re-matches the anchor on every call, so
the button becoming unmatchable *is* the terminating condition and no new
primitive is needed. The cap is a backstop against D5: if the counter
discriminator fails to separate, the loop must still end. It is what stops a
misjudged anchor from burning the 24h run.

The cap is set from the maximum donations actually observed in the capture
session, plus headroom — not from a round number. Hitting it is a bug, so it
logs at warn level rather than passing silently; a task that regularly reaches
its backstop is one whose discriminator has stopped working, and that should be
visible in the 24h run rather than inferred afterwards.

### Migration `00004`

Delete the `radar_quick` and `radar_claim` rows; insert `radar` with a 10800s
cadence and `days_of_week = {1,3,5,6}`. The other four rows are unchanged.

---

## 7. Device-free tests must grow in the same change

`internal/runtime/runtimetest/runtimetest.go` is a second, synthetic registry
that exists so `internal/tasks` tests run with no device and no real templates.
Line 44 currently reads `"alliance": {"help_all_button"}`.

This is the same drift hazard `screens.go` was written to close, one layer
down: **every new screen and anchor needs a synthetic counterpart there, or the
device-free tests quietly stop covering the new code.** Moving `help_all` from
`alliance` to `base` touches `runtimetest.Registry`, `runtimetest.Graph`,
`skeletonScripts` (`tasks_test.go:24`), and `TestSkeletonToleratesMissingAnchor`
(which hardcodes `"alliance"` at `tasks_test.go:79`).

`TestAllTierOneTasksRegistered` (`tasks_test.go:15`) must drop `radar_claim`
and `radar_quick` and gain `radar`.

`runtimetest` also needs synthetic `rewards_banner` and `stamina_prompt`
anchors, so `dismissRewards` and the radar's stamina bail-out are exercised
device-free in both branches: present and absent.

The recognizer needs no new tests: §2's measurement removed the modal rule
before it was written, so `recognizer.go` is untouched by this milestone.

The shared-template region guard (§4) does need its own, and the case that
matters is the one that would otherwise pass by accident: two anchors on one
screen naming the same template with **overlapping** regions must be rejected
at load, while two anchors naming *different* templates may overlap freely —
`base`'s help icon and collect bubble both search most of the screen by design.

Per invariant #6, `make test` continues to pass with no emulator, no adb, and
no Docker.

---

## 8. The capture session

One scripted `agent record` session walking every flow, then labelling and
cropping in `agent studio` in a single pass. One session rather than six
because the popup count has to be known before the vocabulary is designed
around it, and because each trip to the handset is a context switch.

The walk must deliberately capture **both** states of every stateful control:
donate enabled *and* exhausted, radar with Quick Execute *and* with Claim All,
a mailbox with rewards *and* empty, `base` with a collect bubble *and* without,
and `alliance_tech` with the recommendation on the **selected** tab *and* on
the unselected one — the two cases the §6 tab logic branches on, and the only
way to see both badge positions. A single-state capture is how a threshold gets
fitted to a sample of one.

Open questions the session resolves:

1. Is the `CONGRATULATIONS!` banner **identical on the radar and in the
   mailboxes**? The recon captured the celebration in a mailbox only, never on
   the radar, so a shared template is assumed and unverified. If it differs it
   is a second template, never a looser threshold.
2. Does the tech button's **chrome** stay invariant across recommendations?
   Only needed if the badge crop measures too flat (§4) — it is the one
   direction the crop can widen in. Capture the recommendation on several
   different techs in the same session so this is answerable without a second
   trip.
3. Does the collect bubble vanish when there is nothing to collect?
4. Does an empty mailbox's Claim All raise the celebration anyway?
   `dismissRewards` is a no-op either way, so this changes nothing in the code
   — it decides only how many frames of each state the corpus needs.
5. Measured separation between `base`'s two broad-region anchors.
6. Standard deviation of the badge crop against the reference band.
7. D7: does any action animate past the 20s `WaitFor` default?
8. What does the radar show when the **40-task daily cap** is reached (§2)?
   If Quick Execute persists there too, the cap is a second unaffordable-but-
   present state, and the task's "tap it and see" shape already covers it — but
   only if tapping does not open a purchase dialog for that case as well.
9. Capture the **`stamina_prompt`** deliberately: tap Quick Execute while
   stamina is short and hold on the dialog. It needs ≥10 frames and an
   identifying anchor like any other screen. **Take nothing else in that
   dialog** — it spends currency.

---

## 9. Acceptance gate — two phases

The design doc states the M2 gate as *"one account runs unattended 24h, ≥95%
task success, zero stuck-screen incidents."* Per-task success is the right
reading — `help_all`'s volume would otherwise mask a task that fails every
attempt — but per-task percentages are not measurable in a single 24h window:

| task | cadence | runs/24h | what ≥95% would mean |
|---|---|---|---|
| `help_all` | 180s | ~320 | genuinely ≥95% |
| `tech_donate` | 7200s | ~8 | zero failures |
| `daily_gather` | 14400s | ~4 | zero failures |
| `radar` | VS days | ~5, or **0** off-schedule | possibly unmeasurable |
| `mail_collect` | 86400s | **1** | zero failures, n=1 |

At n=1 a percentage is not a measurement. So the gate splits by failure class:

**Phase A — per-task reliability.** `agent run-task` ×20 per task against the
live handset. **≥95% each.** Statistically meaningful, takes hours rather than
days, and a failure points at exactly one task.

**Phase B — 24h unattended.** Tests what only elapsed time can test: **zero
stuck-screen incidents, no unrecovered panic route, correct offline-window
behaviour, no drift.** Overall ≥95%. Started on a VS day so `radar` runs at
all.

Both phases are measured from `task_runs`, which already records a row before
the task acts (`runner.go:33` — a killed process still leaves evidence) and
already distinguishes `succeeded` / `failed` / `paused`. No new
instrumentation.

**`make gate` must also still pass** at 16 screens (§3).

---

## 10. Risks

| risk | mitigation |
|---|---|
| A greyed button matches and every tap silently succeeds | Positive discriminator cropped to include the counter; `tapUntilGone`'s hard cap as backstop; Phase A run against a *known-exhausted* state to prove the loop terminates |
| The two `base` broad-region anchors match each other | Separation measured explicitly between them during the capture session, not only against other screens |
| The badge crop is too flat to discriminate, and cannot be widened toward the tech's contents | Widen toward the button's chrome instead, which is uniform across techs (§4). Measure stddev against the reference band before trusting the crop |
| The tech-list and tab-header regions overlap, so each anchor matches the other's badge | Registry validation rejects overlapping regions for anchors sharing a template on one screen (§4). Without it the failure is silent: the task taps the tab badge believing it is a tech, switches tabs, and reports a donation it never made |
| Radar targets cap out, invalidating the VS-day schedule | Audit item §8.1, resolved before the migration is written |
| Two new screens push the recognizer below 98% | `make gate` is a gate, not a report; `agent score --json` localizes per frame |
| A crash leaves the agent on the stamina dialog or the donate dialog | Both carry a `Back` escape edge, so `NavigateTo` recovers without the panic route. The celebration needs no equivalent: it plays over a screen that stays fully recognizable, so nothing is ever stranded on it |
| **An automated tap spends currency** via the stamina purchase dialog | `stamina_prompt` is a named screen with an identifying anchor and a `Back` escape, and no action anchor at all — there is nothing on it the task is able to tap. It has no in-edge, so `NavigateTo` can never choose a route whose traversal is a purchase (§5) |
| The radar's daily 40-task cap presents as another present-but-unaffordable Quick Execute | Audit item §8.8. The single-pass shape already tolerates it; what must be checked is whether that case also opens a purchase dialog |
| The celebration banner differs on the radar from the mailboxes | Audit item §8.1. A second template, never a looser threshold — one crop stretched to match two different graphics matches neither well |
| The dismiss tap lands after the animation has cleared | Cannot happen: `rewards_banner` is self-gating, so no banner means no tap. This is the failure the rejected back-arrow target would have carried (§6) |
