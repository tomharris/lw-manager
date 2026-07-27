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
five task bodies, a migration reshaping the tasks table, and a two-phase
acceptance gate.

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
| `tech_donate` | tap a tech, donate N times | correct that it is a tree; the tech is chosen by a **red thumbs-up badge**, and the donate button **greys rather than vanishing** |
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

**Resolution:** one `radar` task, scheduled `{1,3,5,6}`, that loops Quick
Execute → Claim All until no targets remain. Targets accumulate untouched on
non-scoring days, which preserves the VS optimization the original schedule was
reaching for, and immediate claiming makes the blocking constraint harmless.
This depends on radar targets accumulating rather than capping — an open audit
item (§8).

---

## 3. Vocabulary: +2 screens, and the M1 gate re-runs

`internal/vision/screens.go` gains:

- **`rewards_popup`** — the overlay raised by Claim All. One name, not two:
  the mail and radar overlays are visually identical, so two labels would put
  contradictory labels on the same pixels and the recognizer would be scored
  wrong on half of them regardless of the crop.
- **`alliance_tech_donate`** — the donate dialog reached from the recommended
  tech.

`internal/vision/corpus_test.go:77` requires **≥10 corpus frames for every name
in `ScreenNames`**, and the gate demands ≥98% across all of them.

> **This milestone does not close until `make gate` passes again at 16
> screens.** The M1 gate is not a one-time event; it is a standing invariant
> that every vocabulary change re-tests.

### Naming a popup removes it from the panic route's care

Today popups are unrecognized, so `recognizeOrRecover` → `panicRoute` → back ×3
dismisses them incidentally (`panic.go:12`: *"Popups and interstitials die to
back"*). Once `rewards_popup` is in `ScreenNames` with an anchor, it recognizes
cleanly, the panic route never fires for it, and `NavigateTo` calls
`Path(rewards_popup, target)` — which returns `ErrNoPath` for a node with no
edges, failing the task.

**A screen we name is a screen we own.** Naming one transfers it from the panic
route's care to the graph's, and the graph must be updated in the same change.

---

## 4. Anchors

| anchor | screen | note |
|---|---|---|
| `help_all_button` | `base` | content-addressed, broad search region |
| `collect_bubble` | `base` | same; must be separable from `help_all_button` |
| `tech_recommended_badge` | `alliance_tech` | see below |
| identifying anchor | `alliance_tech_donate` | |
| `donate_button` | `alliance_tech_donate` | crop **includes the counter** (D5) |
| `quick_execute_button` | `radar` | presence *is* the state |
| `claim_all_button` | `radar` | presence *is* the state |
| `claim_all_button` | `mail_event`, `mail_system` | `mail_alliance` already has one (`manifest.yaml:159`) |
| identifying anchor | `rewards_popup` | |
| `close_button` | `rewards_popup` | |

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

**The badge is tapped *through*.** `Tap` jitters inside the matched box
(`act.go:73`), so anchoring on the thumbs-up alone aims at the badge's few
dozen pixels rather than the tech button's. That works only if the badge sits
inside the button's hit area. If it does not, the fix is **not** an offset — an
offset from a match is a blind tap wearing a disguise, and invariant #3 forbids
it — but a wider crop whose box spans badge and button, keeping the badge as
the discriminating content.

**Small crops are near-degenerate.** The badge is small, and `CLAUDE.md`
records that a nearly-flat template correlates ~1.0 with any similarly flat
region. Measure its standard deviation against the reference band (empty
checkbox 2,346 / checkbox+check 10,324 / wordmark 25,927) before trusting it.

---

## 5. Graph

The three mailbox tap anchors **already exist** (`manifest.yaml:130–155`); only
edges are missing.

```
mail                        → mail_alliance | mail_event | mail_system   (tap)
mail_alliance|event|system  → mail                                       (Back)
alliance_tech               → alliance_tech_donate    (tap tech_recommended_badge)
alliance_tech_donate        → alliance_tech           (Back)
rewards_popup               → radar | mail_alliance | mail_event | mail_system
                                                      (tap close_button)
```

### Popups are graph sinks: out-edges only, never in-edges

BFS cannot route *through* a node it cannot enter. Without this rule a shared
`rewards_popup` with in-edges from four origins becomes a teleporter, making
`base → radar → rewards_popup → mail_alliance` look like a valid path —
a route that requires tapping Claim All on the radar in order to reach the
mail. Tasks open popups by tapping; only `NavigateTo` ever leaves one.

The cost of one shared popup with four escape edges is that `Path` may pick the
wrong origin. `walk`'s `WaitFor` then returns `ErrWrongScreen` and `NavigateTo`
re-plans from where it actually is (`navigate.go:36`). Self-correcting, and
`maxReplans` is 3, so there is headroom.

---

## 6. Task bodies — six become five

No new `Ctx` primitives. Everything composes from `NavigateTo`, `Tap`,
`WaitFor`, `CurrentScreen`, and `Sleep`.

| task | body |
|---|---|
| `help_all` | on `base`, tap `help_all_button`; absent ⇒ success. No navigation. |
| `daily_gather` | on `base`, tap `collect_bubble`; absent ⇒ success |
| `mail_collect` | for each of the three mailboxes: enter, tap Claim All, **optionally** dismiss the popup, back |
| `tech_donate` | `→ alliance_tech`, tap the badge, donate until the counter-bearing anchor stops matching, hard-bounded |
| `radar` | **new**, replaces `radar_quick` + `radar_claim`: loop Quick Execute → poll for Claim All → tap → dismiss → repeat until no Quick Execute |

`help_all` at a 180s cadence is roughly 320 of the day's ~340 runs and now
never leaves `base` — the dominant contributor to the 24h window is also the
cheapest and least failure-prone thing in it.

### Two helpers in `internal/tasks`

**`tapAndDismiss`** — tap an action anchor, briefly poll for `rewards_popup`,
close it if it appeared.

`WaitFor` is the wrong primitive for an optional screen: it polls to a 20s
deadline, runs the panic route, then returns `ErrWrongScreen` (`ctx.go:169`).
Using it for a popup that legitimately may not come costs 20s plus a spurious
back ×3 recovery, three times per `mail_collect` run. The right shape is a
short bounded poll of `CurrentScreen` over ~2s, treating "still on the origin
screen" as *no popup* rather than as a failure. The bound must exist because
the popup animates in, so a single immediate check races it.

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
a mailbox with rewards *and* empty, `base` with a collect bubble *and* without.
A single-state capture is how a threshold gets fitted to a sample of one.

Open questions the session resolves:

1. Do radar targets accumulate across days, or cap out? **Decides whether the
   VS-day schedule is right** (§2).
2. Is the thumbs-up badge inside the tech button's hit area? (§4)
3. Does the collect bubble vanish when there is nothing to collect?
4. Does an empty mailbox's Claim All raise a popup anyway?
5. Measured separation between `base`'s two broad-region anchors.
6. Standard deviation of the badge crop against the reference band.
7. D7: does any action animate past the 20s `WaitFor` default?

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
| The badge is not inside the tech button's hit area | Widen the crop to span badge and button. **Never** an offset from the match — invariant #3 |
| Radar targets cap out, invalidating the VS-day schedule | Audit item §8.1, resolved before the migration is written |
| Two new screens push the recognizer below 98% | `make gate` is a gate, not a report; `agent score --json` localizes per frame |
| Popup left open by a crash strands the next task | `rewards_popup` has escape edges to all four origins, so `NavigateTo` recovers without the panic route |
