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
The panic route behaved correctly throughout: three back presses, then an app
restart (ten of them, all logged), then it *reported* rather than acting. It
never tapped a screen it could not identify, so invariant #3 held under four
hours of sustained failure, and no run ever hung — zero stuck rows.

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

Root cause of the outage is not established. It resolved without intervention
and nothing in the log names a trigger — the plausible candidates (a game
maintenance window, an OS dialog, the screen locking) are not distinguishable
from what was recorded, because nothing captures a frame on the unrecognized
path. **That is the actionable gap: the panic route should blob a screenshot
before it gives up.** Without it a four-hour incident leaves no evidence beyond
its own error string.

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

## The plan's Step 5 greps for a string the code never logs

Step 5 counts restart recoveries with:

```bash
grep "panic route: recovered via restart" /tmp/m2-24h.log
```

That matches **zero** lines. The messages the runtime actually emits are
`panic route: back exhausted, restarting app` and `app restarted`, each
appearing **10 times** in this run. Followed literally, the step reports no
restart recoveries during an incident that had ten — the precise signal it
exists to surface. Counts below come from the real strings.

| signal | count |
|---|---|
| `panic route: pressing back` | 30 |
| `panic route: back exhausted, restarting app` | 10 |
| `app restarted` | 10 |
| `scheduler tick error` | 10 |

Ten restart recoveries is not ordinary. Per the plan's own standard — "back
press recoveries are ordinary, restart recoveries are not" — this run warrants
investigation despite passing, and the missing screenshot-on-panic is what
prevents it.

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
3. The panic route discards the evidence it most needs — blob a frame before
   giving up.
4. A ratio gate cannot see a blackout; assert on the longest success-to-success
   gap too.
5. `mail_collect` ran once and failed (inside the outage), so its once-daily
   path still has no successful unattended observation.
