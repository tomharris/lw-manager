# M2 Phase A — per-task reliability on the handset

**Run:** 2026-08-10, 12:54–13:55 UTC
**Device:** moto g play 2024, `ZL8326G8MZ`, 720x1600
**Account:** 16 (`lw34alt`, `alliance_data`)
**Method:** 20 consecutive `agent run-task` invocations per task, jittered 3–8s
between runs. `task_runs` was empty beforehand, so nothing else is in the window.

The plan specified a fixed `sleep 5` between runs. That was replaced with
jitter: 100 runs on a perfect five-second cadence is precisely the signal
invariant #7 exists to suppress, and the measurement is indifferent to it.

## Result: FAILED as measured — one task below gate

| task | ok | failed | paused | stuck | total | % |
|---|---|---|---|---|---|---|
| `help_all` | 21 | 0 | 0 | 0 | 21 | 100.0 |
| `daily_gather` | 20 | 0 | 0 | 0 | 20 | 100.0 |
| `mail_collect` | 20 | 0 | 0 | 0 | 20 | 100.0 |
| `tech_donate` | 20 | 0 | 0 | 0 | 20 | 100.0 |
| **`radar`** | **16** | **4** | 0 | 0 | **20** | **80.0** |

(`help_all` has 21 because a single smoke run preceded the matrix.)

Four tasks clean. `radar` at 80.0% against a ≥95% gate. Zero `paused` rows and
zero `stuck` rows anywhere.

The percentage query in the plan divides successes by *all* rows, which quietly
counts a kill-switch `paused` row as a reliability loss. Phase B's query already
breaks `paused` out into its own column; that shape was used here instead. It
made no difference this run — nothing paused — but it would have, silently.

## The radar failures were false negatives

All four are the same error:

```
tasks: Claim All never appeared after an execution: tasks: claim never appeared
```

They are not distributed randomly. Runs 1, 3, 5, 7 failed, strictly alternating
with successes, and runs 9–20 were then clean:

| run type | duration | what it did |
|---|---|---|
| failing (1,3,5,7) | ~85s (first 122s) | executed, polled, gave up |
| success after a failure (2,4,6,8) | 25.0, 25.0, 24.9, 25.0 | **claimed a banked reward**, then found nothing to execute |
| no-op (9–20) | 10.5s ×10, metronomic | nothing banked, no targets |

A run with nothing banked and nothing to execute costs 10.5s. Every run
following a failure cost 25.0s — a consistent **+14.5s**, which is the
claim-and-dismiss path. The only thing that could have banked that reward is
the immediately preceding "failed" execution.

**So Claim All always appeared. It appeared later than the poll budget, and the
next run collected it.** No reward was lost in any of the four cases. That is
`radarPass`'s claim-first ordering (`radar.go`, "Claim-first is load-bearing")
working exactly as designed. The `failed` verdict was recorded against work
that had in fact completed.

### Root cause: the budget was counted in the wrong unit

`claimPollAttempts = 8` reads as a small, obviously-safe number, and its comment
claimed the game reveals Claim All "a few seconds after an execution". Neither
is true on real hardware. Each attempt costs a 720x1600 screencap over adb plus
a full NCC recognition pass across 16 screens — about **9.4s**. Eight attempts
was therefore **75–109s** of wall clock, a figure no reader of the constant
could see.

The measured reveal is **90–125s** after the execute tap. The budget sat just
below it.

The systematic "just barely missed" pattern — four times out of four — is not a
coincidence and was the clue that identified the mechanism. The poll budget and
the reveal are both anchored to the same execute tap, so a budget slightly
under a roughly fixed reveal duration falls short *every* time, and the next
run, starting ~30s later, always lands past it.

An alternative hypothesis — that Claim All only renders on a fresh entry to the
radar screen, so polling in place could never see it — was **refuted**:
`NavigateTo` returns immediately when already on the target
(`internal/runtime/navigate.go:23-24`), so the succeeding runs never re-entered
the screen, yet saw the claim. Polling in place does work; the bound was simply
too short.

### Fix

`claimPollAttempts` (a count) is replaced by `claimPollBudget` (a wall-clock
duration, 4 minutes), checked after each poll in the same shape `WaitFor` uses
at `internal/runtime/ctx.go:153`. An attempt count means whatever the device's
screencap speed makes it mean; a duration means what it says.

`ErrClaimNeverAppeared` is deliberately **kept** as a genuine fault signal. The
recon (`2026-07-27-m2-recon-findings.md`) had recommended demoting a missing
claim to an ordinary outcome, but its stated reason was insufficient stamina,
which is now caught explicitly by the `stamina_prompt` branch. Lateness was the
second legitimate cause it did not enumerate, and lateness is fixable by
budgeting correctly rather than by discarding the signal.

Covered by `TestRadarPollsPastTheOldAttemptCount`, which fails with the exact
production error against the old bound.

### Not re-measured on hardware

Radar targets were exhausted by run 9 and refill on a 6-hour interval, so the
has-work path could not be re-exercised in this window. **The fix is verified
device-free only.** Phase B spans 24 hours and will cross roughly four refills,
which is where it gets its real-hardware confirmation — that is the specific
thing to check in the Phase B results.

## D7 (§8.7): no `WaitFor` timeout signature

| task | slowest | mean |
|---|---|---|
| `mail_collect` | 2:52.4 | 1:37.1 |
| `radar` | 2:02.0 | 0:30.7 |
| `tech_donate` | 0:47.1 | 0:12.0 |
| `daily_gather` | 0:08.8 | 0:08.7 |
| `help_all` | 0:07.9 | 0:07.4 |

Nothing clusters on a multiple of the 20s `waitTimeout`. `radar`'s 2:02 is the
claim poll described above, not a wait timeout. **No change to
`Options.WaitTimeout` is warranted.**

`mail_collect` is by far the slowest task (mean 1:37, slowest 2:52) while being
100% reliable, so it is a scheduling cost rather than a defect — worth
remembering when its cadence competes with another task for the device.

## Gate

Phase A **does not pass as measured**: `radar` 80.0% against ≥95%.

It is recorded as a **conditional pass** to proceed to Phase B, on the explicit
basis that the four `radar` failures are understood, are false negatives that
cost no rewards, and are fixed. The other four tasks are at 100% over 81 runs
with zero stuck and zero paused rows. Radar's real-hardware number is owed by
Phase B.
