# M4 recon — the two collection routes, on the handset

**Date:** 2026-08-12
**Device:** moto g play 2024, `ZL8326G8MZ`, 720x1600
**Account:** 16 (`lw34alt`), alliance `[OrCa] Organized Chaos`, 96/100 members
**Game version:** 1.0.357[1854], assets 2327.1490_803F_38612.23485
**Method:** manual adb navigation, frames in `evidence/m4-recon-2026-08-12/`

Run before finalizing the M4 design, on the M2 precedent: recon first, design
against real frames. It corrected four assumptions the design was resting on,
and two of them would have produced a silently wrong roster rather than a
failure.

---

## 0. The handset was locked, and `stayon` did not prevent it

Before any game screen could be reached, the device was found dozing behind a
keyguard. Every precondition CLAUDE.md names was satisfied:

| signal | value |
|---|---|
| `mStayOn` | `true` |
| `stay_on_while_plugged_in` | `15` (all four sources) |
| USB powered | `true` |
| battery | `status: 5` (FULL), `level: 100` |
| uptime | 13 days — no reboot |
| user state | `RUNNING_UNLOCKED` — a re-lock, not a post-boot lock |

CLAUDE.md calls `stayon` "**the load-bearing one**," and the entire M2
display-sleep remediation rests on it. **That claim now has a counterexample on
this handset.** The mechanism is unknown — Motorola's `com.motorola.actions`
power management is the obvious suspect — and per the discipline the radar fix
established, the response must not depend on knowing why.

The credential has since been cleared (`locksettings clear`), which is the
mitigation CLAUDE.md already recommends. This finding is what makes it
mandatory rather than advisory: `Wake` turns the display on and lands on the
keyguard, so the M2 recovery story holds only while the device never locks.

### A secure surface returns a zero-byte capture, not a black frame

On the PIN entry screen, `adb exec-out screencap -p` returned **0 bytes**.
After `KEYCODE_BACK` backed out to the lock screen, the same command returned
845,330 bytes.

This is a third failure mode, distinct from the two already documented:

| situation | `screencap` yields | fails at |
|---|---|---|
| display asleep | well-formed all-black PNG | recognition — matches no anchor |
| `FLAG_SECURE` surface | **nothing at all** | PNG decode, before recognition |
| mid-transition | well-formed all-black PNG | recognition |

The third row is also new. Immediately after the keyguard was dismissed,
`mWakefulness=Awake`, `mScreenState=ON` and `mStayOn=true` all reported healthy
while `screencap` returned a solid black frame; `KEYCODE_HOME` then rendered
normally. **A black frame is therefore not sufficient evidence of a sleeping
display** — it is also what a transition looks like — which weakens the
inference the Phase B root-cause analysis drew from exactly that signal.

---

## 1. Route: roster (Alliance → Members)

### The member count lives on `alliance`, not on `alliance_members`

`alliance` carries `Members: 96/100` (frame 01), along with the alliance tag,
name, leader and power. `alliance_members` carries no count at all. The design
had assumed a header count on the members screen; the reconciliation ground
truth is one screen higher, and it is a single stable non-scrolling read.

`alliance` is also where the `alliances` row comes from — tag, name, leader.

### `members_button` must be cropped; it does not exist yet

`alliance` currently declares only `alliance` and `tech_button`. The Members
button is a clean text-plus-icon target at roughly (526, 824).

### The member list is not a flat list — it is five collapsible rank groups

This is the finding that most changes the route. `alliance_members` is
structured as:

```
[ sticky header block: title, announcement, R5 president card,
  four officer cards, search box ]        <- does not scroll
[ R4 "This Is It"   2/9   ▲ ]            <- collapsed
[ R3 "Footloose"    8/64  ▲ ]            <- collapsed
[ R2 "I'm Alright"  2/11  ▼ ]            <- expanded
[   member row ] x 11
[ R1 "Danger Zone"  1/11  ▲ ]            <- collapsed
```

Chevron `▼` means expanded, `▲` collapsed; tapping a group header toggles it
(frame 03, where tapping R3 expanded its 64 rows). Group counts read
`online / total`, and **the totals sum to the alliance member count**:

```
R5 1  +  R4 9  +  R3 64  +  R2 11  +  R1 11  =  96      = "Members: 96/100"
```

Three consequences:

- **The route is expand-and-scroll per group**, not one scroll. Four groups in
  the list plus R5, which is read from the president card rather than a row.
- **Reconciliation is per group, not global.** Each header states its own
  expected total, which localizes a shortfall to one group instead of leaving a
  single global number to explain.
- **A member row does not carry its own rank.** Rank comes from the enclosing
  group. This would have been a serious problem for position-independent
  dedupe — except that the group header is *sticky*: it pins to the top of the
  scroll region and is therefore present in every frame of that group's
  capture, carrying the rank with it.

### The sticky group header occludes the row beneath it

Visible in frame 02, where a `Manage` button peeks out from under the pinned
R2 header. Segmentation must discard the top partial row rather than parse it.

### Row fields, and two precision problems

Each member row is ~112px tall and carries: avatar, name, `Power: 216.2M`,
`Lv.35`, a relative last-active (`Online`, `1h ago`, `5h ago`), and a `Manage`
button (absent on the logged-in account's own row).

- **Power is abbreviated to four significant digits** — `216.2M`, not a raw
  number. Any "power growth Δ/week" metric inherits ±50,000 of quantization,
  which is below the weekly deltas that matter but must be recorded as a
  parsed-precision limit rather than discovered later.
- **Last-active is relative**, so the fact must be stored as
  `observed_at − relative`, and its resolution is an hour at best. `Online`
  has no numeric value at all.

Names confirm the fuzzy-matching case comprehensively: `ΔΚΔŽΔ`,
`M I C H E L L` (letter-spaced), `Zero^Orca` (superscript), `Aureum ⊂👑`,
`𝕝𝕝 Leo 𝕝𝕝`.

---

## 2. Route: VS ranking (base → VS → Ranking → Weekly → Your Alliance)

### The route is confirmed, and it is not reachable from Alliance

`base → VS` lands on **ALLIANCE DUEL**, whose bottom bar has a `Ranking`
button; that opens **RANKING**, which carries the `Daily Rank` / `Weekly Rank`
tabs and a `Your Alliance` checkbox at bottom right. This matches the route
CLAUDE.md documents and confirms again that the "Alliance → Members → VS
Ranking" description is wrong.

### `Your Alliance` is the near-degenerate anchor, and it is a tap target

Frames 04 and 05 show it unchecked and checked. It is literally an empty
checkbox — which is exactly the near-degenerate template CLAUDE.md measures at
stddev 2,346, ~11x flatter than a text anchor, reading
`worst-in 1.000 / best-out 1.000`.

CLAUDE.md discusses this only as a *scoring* problem. M4 needs to **tap** it,
and there the same flatness fails worse: NCC will locate its best match at an
arbitrary smooth region and the task will tap there while invariant #3 believes
an anchor was matched. That is a blind tap wearing a matched anchor's clothes.
**Recrop to include the adjacent "Your Alliance" label**, so the template
carries text variance and localizes.

### Daily and Weekly have different layouts

`Daily Rank` carries a second tab strip of weekday buttons (`Mon.` … `Sat.`);
`Weekly Rank` has no such strip, so its list starts ~90px higher. The two
cannot share a list region.

### The ranking omits zero-score members — the design assumption was wrong

The weekly list ends at **rank 94** (frame 06) against **96** alliance members.
On the daily tab, the logged-in account showed rank `Unlisted` with `0` points
(frame 04).

**So the ranking lists only members with a nonzero score.** The design had been
scoped on the answer "ranking shows all", and reconciliation was to be a clean
equality against roster size. It cannot be. The rule must be:

> A member absent from a complete weekly capture is recorded as zero, **not**
> as missing — but only when the capture reached the bottom of the list. An
> incomplete capture makes absence and zero indistinguishable, which is
> precisely the silent under-reporting invariant #4 exists to prevent.

This makes the bottom-of-list proof load-bearing for correctness, not merely
for completeness.

### The self row is pinned and therefore appears twice

The logged-in account's row is pinned below the list (frame 05: rank 84,
`mini tomx1000`) and *also* appears in its natural position in the list. It is
outside the scroll region and never moves.

Two fixes, both wanted: **exclude the pinned band from the list region**, and
keep the identity cross-check, which is the only mechanism that would catch a
duplicate arriving at two different screen positions. Geometric dedupe alone
cannot.

---

## 3. The scroll gesture, measured

Rows are 128px on the ranking screen. The usable list region is roughly
y ∈ [295, 1285] — below the column header, above the pinned self row — so
about 990px, or 7.7 rows.

| gesture | content moved | vs. 990px viewport |
|---|---|---|
| 700px over 300ms | ~1504px (~11.6 rows) | **exceeds it — rows never displayed** |
| 300px over 800ms | ~512px (~4 rows) | ~48% overlap |

**Fling roughly doubles the gesture distance**, and the first row of that table
is what a naive implementation would do. Scrolling the full list that way
reaches the bottom in 8 swipes and never photographs perhaps a third of the
members — while every frame looks perfectly valid.

This is direct empirical support for measuring the offset rather than assuming
it, and for the `offset > usableHeight ⇒ rows were never on screen` check: that
check is not a defensive edge case, it fires on the obvious gesture.

**Recommended starting parameters:** 300px over 800ms, giving ~48% overlap
against a 30% target, with the measured offset as the authority.

### Bottom detection must be measured inside the list region

Frames 8 and 9 of the fast scroll were byte-identical, so whole-frame equality
appeared to work. It does not generalize: frame 07 caught a **scrolling
announcement banner** ("...공포 appointed RealBoy as Secretary of Interior!")
animating in the header. While that banner runs, consecutive frames differ
even when the list has not moved, and whole-frame comparison would report
progress forever.

`ScrollOffset(prev, cur, listRegion)`'s region parameter is therefore
load-bearing rather than incidental. Note also that this screen is otherwise
fully static — unlike radar, whose sweep animation would defeat frame equality
outright.

---

## 4. Net effect on the design

| assumption | verdict |
|---|---|
| member count is on `alliance_members` | **wrong** — it is on `alliance` |
| roster is one scrollable list | **wrong** — five collapsible rank groups |
| ranking lists every member | **wrong** — scorers only, 94 of 96 |
| ~30% overlap follows from a swipe | **wrong** — fling overshoots the viewport |
| measured offsets beat assumed ones | **confirmed, empirically** |
| identity cross-check is worth keeping | **confirmed** — the pinned self row |
| `vs_ranking_alliance_button` needs a recrop | **confirmed, and it is a tap target** |

New anchors to crop: `alliance/members_button`, `vs_ranking/weekly_tab`,
`alliance_members` group-header and row markers, and a recropped
`vs_ranking_weekly/vs_ranking_alliance_button` carrying its label.

Two screens seen in recon are not in the vocabulary: the **Alliance Duel**
screen that `base → VS` actually lands on, and the ranking screen's daily
weekday strip. The former needs a name, since the route taps through it.
