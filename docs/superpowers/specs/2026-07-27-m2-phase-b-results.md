# M2 Phase B — the 24-hour unattended run

**Window:** 2026-08-10T14:51:12Z → 2026-08-11T14:51:12Z (exactly 24h)
**Device:** moto g play 2024, `ZL8326G8MZ`, 720x1600
**Account:** 16 (`lw34alt`, `alliance_data`)
**Command:** `agent run` (scheduler loop), detached, single attached device

The plan bounds the measurement with `max(started_at) - interval '24 hours'`,
which floats relative to whatever row happens to be last. Phase A's 101 rows and
a kill-switch pre-flight row were already in the table, so the window was pinned
to an explicit start timestamp instead.

## Gate: PASSED

| task | ok | failed | paused | stuck | total |
|---|---|---|---|---|---|
| `help_all` | 219 | 6 | 0 | 0 | 225 |
| `tech_donate` | 4 | 2 | 0 | 0 | 6 |
| `daily_gather` | 3 | 1 | 0 | 0 | 4 |
| `radar` | 3 | 0 | 0 | 0 | 3 |
| `mail_collect` | 0 | 1 | 0 | 0 | 1 |
| **overall** | **229** | **10** | **0** | **0** | **239** |

**95.82% succeeded, zero rows still `running`.** Both gate conditions met:
≥95% and zero stuck-screen incidents.

Read the rest of this document before treating that as a clean bill of health.

## The percentage is not availability

All ten failures are one contiguous incident, 01:55 → 05:52 EDT, and every one
carries the identical error:

```
runtime: account 16 unrecoverable after 3 backs and a restart:
runtime: lost - no screen recognized after recovery
```

The agent recovered on its own at 06:43 and ran clean to the end of the window.
The panic route ran its full ladder each time — three back presses, then an app
restart, then it *reported* rather than acting. It never tapped a screen it
could not identify, so invariant #3 held under four hours of sustained failure,
and no run ever hung — zero stuck rows. It also never actually recovered
anything; see "The panic route recovered nothing" below, which is a separate
and more serious finding than the outage itself.

The `help_all` failures were spaced 8m → 13m → 24m → 49m → 1h37m, each roughly
double the last. That is `backoff.go` (`cadence * (2^failures - 1)`) doing its
job. It also means **the success percentage measures attempts, not uptime.** At
a 180s cadence `help_all` would have made ~80 attempts during the outage;
backoff reduced that to 6. The same four-hour blackout without backoff would
have scored near 75% and failed the gate.

So 95.82% and "a four-hour outage" are both true. The metric and the backoff
interact, and the percentage alone cannot surface a blackout. **A future gate
should assert on the longest gap between consecutive successes, not only on a
ratio.**

> **Root cause established 2026-08-11.** It was the display going to sleep.
> Once the panic route was made to retain its frame, the failure reproduced on
> demand and the retained frame was solid black — a sleeping screen, which
> `screencap` returns as all black and which matches no anchor. Neither a back
> press nor an app restart turns a screen on, so the ladder could not have
> recovered. See "The display was asleep" below.

When this was first written the root cause was *not* established: nothing in
the log named a trigger, and the plausible candidates (a game maintenance
window, an OS dialog, the screen locking) were indistinguishable from what had
been recorded, because nothing captured a frame on the unrecognized path.
**That was the actionable gap, and closing it answered the question within
minutes** — which is the argument for the fix better than any reasoning about
it.

## The offline window ran in UTC, not local

Observed quiet period: **19:37 → 01:55 EDT**, 6h18m, matching the derived 5–7h
window in duration.

But it should not have been at that hour. `OfflineWindow` derives the window
from `hash64(accountID, date)` in `date.Location()`, and `Plan`'s contract
(`schedule.go:55-56`) states plainly that `now` arrives "already in the
operator's location … so `now.Weekday()` and the offline-window date are the
operator's local day."

The caller does not honour its own callee's contract:

- `loop.go:32` — `Location *time.Location // default time.UTC`
- `loop.go:75-76` — `if l.loc == nil { l.loc = time.UTC }`
- `cmd/agent/main.go:451` — builds `scheduler.Options` without `Location`

So `now` reaches `Plan` in UTC. The observed window is exactly 23:41 → 05:55
**UTC**, which is 19:37 → 01:55 EDT. Two consequences, both observed:

1. **The offline window is inverted relative to its purpose.** It takes the
   device offline through the operator's evening and runs it at 02:00–06:00
   local. For a detection-avoidance feature this is close to backwards: the
   account goes quiet when real players are active and plays while they sleep.
2. **`radar`'s weekday gate `{1,3,5,6}` is evaluated in UTC**, so Monday ended
   at 20:00 EDT. The fourth radar run expected near 22:07 EDT never fired, and
   that cost the run its best chance to exercise the radar fix.

This is a one-line fix (pass the operator location into `scheduler.Options`)
but it is a behaviour change to the detection-avoidance schedule, so it is
recorded here rather than folded into the M2 gate.

## The panic route recovered nothing, in any of the ten incidents

Step 5 counts restart recoveries with:

```bash
grep "panic route: recovered via restart" /tmp/m2-24h.log
```

That matches **zero** lines. An earlier revision of this document read that as
a broken step — a grep for a string the runtime never logs. **That was wrong.**
The runtime does emit it, at `panic.go:57`, when a restart recovers. It never
recovered.

| signal | count |
|---|---|
| `panic route: pressing back` | 30 |
| `panic route: recovered` (via back) | **0** |
| `panic route: back exhausted, restarting app` | 10 |
| `app restarted` | 10 |
| `panic route: recovered via restart` | **0** |
| `scheduler tick error` | 10 |

30 back presses across 10 incidents is exactly 3 each, and `panicRoute` returns
the instant a back press recovers — so **no back press recovered either**, or
the total would be below 30. Every incident ran the ladder to the bottom: three
backs, a restart, the full `restartTimeout`, then `ErrLost`.

So the panic route has **no observed successful recovery at all** across 24
hours. The agent's return to health at 06:43 was not the route succeeding; it
was whatever had broken recognition going away on its own.

What the route did do correctly is refuse to act. It never tapped a screen it
could not identify, which is invariant #3 holding under precisely the
conditions designed to break it. That is worth stating separately: **the
route's safety property is proven; its recovery property is unproven and
currently measures zero.**

This strengthens the case for screenshot-on-panic rather than weakening it. A
recovery ladder with a 0/10 success rate cannot be tuned — more backs? a longer
`restartTimeout`? a different entry screen? — without knowing what was on the
display, and nothing captured it.

## The radar fix from Phase A does not work

Phase A concluded that `claimPollAttempts = 8` was too short and replaced it
with a 4-minute wall-clock `claimPollBudget`. **That conclusion was wrong**, and
three manual `run-task` validations after the window closed disproved it:

| run | duration | outcome |
|---|---|---|
| 346 | 41s | succeeded |
| **347** | **251s** | **failed — `ErrClaimNeverAppeared`** |
| 348 | 26s | succeeded |

Run 347 consumed the entire four-minute budget and still never saw Claim All;
run 348, seconds later, "claimed" in 26s. That is the same alternating pattern
Phase A saw, now reproduced against a budget four times larger. A reveal
landing in the ~60s gap right after a four-minute poll is not credible as
latency, which is what Phase A's reading required.

A screenshot taken immediately after run 348 shows why. The device sits on an
undismissed celebration panel — **"CONGRATULATIONS! / Here are all the rewards
you received this time"** — with a grid of granted rewards. The same frame also
reads "You can complete 1 tasks, requires 10 Stamina" (stamina 51, so
affordable), shows Quick Execute present, and "13 Radar Task(s) will be
restored in 04:33:49", confirming the 6h refill.

> **Corrected 2026-08-11 by frame-by-frame recon.** The claim below — that the
> flow "never reveals a Claim All button" — is **too strong**. A burst capture
> of one full radar run (evidence in `evidence/radar-2026-08-11/`) shows the
> flow does produce a claim control. See "What the radar flow actually does"
> at the end of this document. The observed defect is a race, not an absence.

**The flow model in `radar.go` is wrong.** Quick Execute grants its rewards
directly through that celebration panel; it does not reveal a Claim All button.
`Claim All` belongs to radar tasks that completed *passively* in the background
— the context the recon observed it in (`2026-07-27-m2-recon-findings.md`: "11
task rewards can be claimed") and then generalized to the post-execute case.
So `claimWhenReady` polls for a control that flow never produces, and the
budget's size is irrelevant.

Two further defects are visible in that one frame:

- **`dismissRewards` silently no-ops.** It taps `rewards_banner` once and
  returns nil when the anchor does not match (`helpers.go:67-70`). The banner
  is still on screen after a run that reported success, so the anchor does not
  match this celebration layout. Its "self-gating" property means a
  non-matching anchor is indistinguishable from nothing to dismiss.
- **The agent leaves the device on a modal panel**, not on an idle screen, so
  the next task inherits a non-idle state.

### Status of the Phase A change

`claimPollBudget` is *retained but is not a fix*. A duration is still the honest
unit — an attempt count silently meant whatever the handset's screencap speed
made it mean. But it now makes each doomed radar run cost four minutes instead
of 85s, which is worse operationally until the flow model is corrected. **Lower
it or short-circuit the post-execute wait as part of the redesign.**

**`radar` is an open defect.** Its 3/3 in the table above is not evidence of
health: none of those runs exercised the execute-then-collect path.

## Verdict

The M2 phase gate **passes**: 95.82% ≥ 95%, zero stuck rows, and the panic
route never violated invariant #3 across a four-hour outage.

Carried forward, none blocking the gate:

1. `radar`'s post-execute flow model is wrong — redesign against the
   celebration panel, and re-verify the `rewards_banner` anchor. Effectively a
   revision of Task 14.
2. The scheduler runs on UTC while documenting operator-local, inverting the
   offline window and mis-gating radar's weekdays.
3. The panic route recovered 0 of 10 incidents, and discards the evidence
   needed to say why — blob a frame before giving up.
4. A ratio gate cannot see a blackout; assert on the longest success-to-success
   gap too.
5. `mail_collect` ran once and failed (inside the outage), so its once-daily
   path still has no successful unattended observation.

## What the radar flow actually does (recon, 2026-08-11)

One radar run was burst-captured at 2s intervals while `agent run-task
--account 16 --task radar` executed (run 350, succeeded, 42s). Frames are in
`evidence/radar-2026-08-11/`. Deliberately not `agent record`: that rewrites
`fixtures/corpus/index.yaml` from the working tree, and this corpus had not
been pulled here.

The observed sequence:

1. **`01-radar-quick-execute.png`** — radar screen. "13 Radar Task(s) will be
   restored in 02:21:47", "You can complete 1 tasks, requires 10 Stamina",
   stamina 77, `Quick Execute` present, no claim control.
2. **`02-post-execute-scattered-rewards.png`** — after the tap. Stamina 77 →
   67 and XP 1,987 → 1,992, so the execution ran. The rewards appear as
   **loose items scattered across the map**, `Quick Execute` is gone, and a
   small **collect tray sits at bottom right**. Still no button labelled
   Claim All, but there *is* a claim control.
3. **`03-congratulations-after-collect.png`** — after the tray is tapped, the
   CONGRATULATIONS panel with the reward grid.

Two corrections to the section above fall out of this:

- **The flow does produce a claim control.** "Never reveals a Claim All
  button" was over-generalized from a single screenshot taken after the fact.
  The control is the collect tray, and the `claim_all_button` anchor evidently
  matches it — run 350 collected successfully.
- **The celebration is a modal with a scrim, not "a transient animation over
  its origin screen".** `helpers.go:53-57` states the opposite, and frame 03
  shows the map clearly dimmed behind it. That premise is what justifies
  `dismissRewards` tapping the banner itself and shrugging when it is absent.

### The observed defect is a race

The run reported `succeeded` at 13:23:01. The CONGRATULATIONS panel rendered at
**13:23:04** — three seconds *after* the task had finished. So `dismissRewards`
looked for `rewards_banner` before the panel existed, matched nothing, and
returned nil, which its self-gating design cannot distinguish from "nothing to
dismiss". The device was left on the panel, which is exactly the leftover state
recorded earlier in this document.

That also supplies a mechanism for Phase A's alternating pass/fail that does
not require the reveal-latency story: a run that leaves the panel up hides
`Quick Execute` from the next run, and a run that clears it does not.

**This is still not enough to redesign against.** What is missing is a
burst-captured *failing* run — run 350 succeeded, so the frames above show the
happy path only. `ErrClaimNeverAppeared` has not yet been observed frame by
frame, and two previous radar diagnoses were wrong precisely because they
reasoned from durations and single screenshots instead of the sequence. The
next radar refill is the opportunity; the recon harness is
`scratchpad/recon.sh` in shape and takes about a minute to re-point.

Until then `radar` remains an open defect, and `claimPollBudget` should be
lowered from 4 minutes so a doomed run fails fast rather than slowly.

## The display was asleep

With the panic route retaining its frame, the outage reproduced on the first
attempt. Letting the handset doze and then running any task gives the outage's
error string verbatim:

```
runtime: account 16 unrecoverable after 3 backs and a restart:
runtime: lost - no screen recognized after recovery
```

The retained frame is **solid black**, and the content-addressed blob store
deduplicated it onto an object first written on **26 July** — so this exact
frame is a recurring condition, not a one-off.

The mechanism is complete and leaves nothing to infer:

| step | measured |
|---|---|
| display asleep | `screencap` returns 7,904 bytes, all black |
| a black frame | matches no anchor → `ErrNoScreenRecognized` |
| `Back` | `input keyevent KEYCODE_BACK` — does not wake a display |
| `AppRestart` | `am force-stop` + `monkey` — does not wake a display |
| after `KEYCODE_WAKEUP` | 812,945 bytes, a real frame |

So 0/10 was structural. No rung of the ladder could turn a screen on, and the
route was retrying the only three things that cannot help.

### The code fix is necessary but not sufficient

`Transport.Wake` now runs as rung zero, before the back presses it would
otherwise render useless. On hardware that converts the black frame into a real
one — and the run still fails, for two reasons the runtime cannot fix:

1. **The handset re-locks on sleep, and adb cannot dismiss its keyguard.**
   `input keyevent KEYCODE_MENU`, `wm dismiss-keyguard`, a swipe, and a power
   toggle all leave `isKeyguardShowing=true`. What the recognizer then sees is
   the lock screen, which matches no game anchor. `locksettings get-disabled`
   reports `false` — this device has a keyguard configured.
2. **The display re-sleeps during the 93s restart wait**, so even a successful
   wake is undone before the route finishes.

Both halves are operational, and belong in the handset's setup rather than in
the runtime:

```bash
adb shell svc power stayon true      # do not sleep while charging
adb shell locksettings set-disabled true
```

`Wake` is still worth having: it is what makes a dozing display recoverable at
all on a handset configured this way, and it is the difference between a black
frame and a real one. But **the M2 24-hour run's outage would not have been
prevented by code alone**, and a future unattended run should assert the
device's power and keyguard settings before it starts rather than discovering
them four hours in.
