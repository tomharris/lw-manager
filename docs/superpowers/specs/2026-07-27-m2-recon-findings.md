# M2 reconnaissance findings

**Date:** 2026-07-27
**Task:** Task 1 of `docs/superpowers/plans/2026-07-27-m2-tier1-tasks.md`
**Corpora:** `/tmp/recon-radar` (68 frames), `/tmp/recon-scrim` (76 frames),
moto g play 2024, 720×1600, game version 1.0.354

Two questions were asked because either answer could invalidate a design
decision rather than merely fill a blank. Both did.

---

## 1. There is no scrim, and no modal — verdict: `modal` NOT needed

The rewards display raised by Claim All is a **transient celebration
animation**, not a dialog.

| expected | observed |
|---|---|
| dimmed scrim over the origin | **no dimming at all** — background at full brightness |
| a close/X button | **none anywhere** |
| origin partly obscured | origin **fully visible**: `ALLIANCE` header, Search bar, `Delete All`, `Claim All` all unobscured |
| dismissed by tapping close | **dismissed by tapping anywhere** |

The spec's argument for modal precedence was: the scrim is invisible to NCC,
so both the origin and the popup qualify, so precedence rather than confidence
must decide. **The premise was false.** There is no scrim to be invisible to.

The algebra in the spec (`p → αp + (1−α)c` leaves NCC mathematically unchanged)
is correct and irrelevant — it reasons carefully about a world this game does
not implement. That is the whole reason §8.8 said *measure it rather than trust
the derivation*: a derivation can be sound and still be about the wrong thing.

**An overlay frame is not "the popup over the mail screen". It genuinely is
the mail screen, with an animation playing on it.** Recognition is already
correct; nothing needs to change to make it so.

### Consequences

- **Delete** the `rewards_popup` screen, the `modal` manifest field and
  recognizer precedence rule, the `close_button` anchor, the four escape
  edges, and `dismissModal`.
- **Keep** the shared-template region guard — it exists for the tech badge and
  is independent of any of this.
- Overlay frames are labelled as their **origin screen** in the corpus, which
  is what they are.

### But the animation still needs a tap, and that is not free

Tap-anywhere dismissal is still an action, and invariant #3 forbids acting
without a matched anchor. "Anywhere" is not a coordinate we are allowed to
express.

The origin screen is recognized throughout, so the task can tap **a designated
inert anchor on the origin screen** — screen furniture chosen for being stable
and non-interactive. On `mail_alliance` the `ALLIANCE` wordmark is the obvious
candidate: it is a label, so a tap landing there after the animation has
already cleared does nothing.

**This is not automatically safe on every screen.** The radar's HUD carries a
stamina meter that almost certainly opens a purchase dialog when tapped, and
its map area is covered in tappable mission pins. Each screen that can raise
the animation needs a *verified inert* tap target, and "verified" means
observed, not assumed. Added as an audit item.

---

## 2. The radar is stamina-gated, and Quick Execute lies

Observed directly, in one frame:

> **"You can complete 6 tasks, requires 60 Stamina"** — with **⚡31** in the HUD,
> and **Quick Execute present and rendered enabled**.

So the button's presence does not mean the action can succeed. **Tapping it
with insufficient stamina opens a buy/refill prompt.**

A second frame shows **"11 task rewards can be claimed"** with Claim All, so
rewards bank in quantity. A third HUD element reads `x/40` beside a *"Fully
restore at 7-28 09:04:33"* timer — a daily cap on top of the per-task stamina
cost. Neither the cap nor the cost appears anywhere in the spec.

### Consequences

- **`radarSweep`'s terminator is wrong.** The plan loops until Quick Execute is
  *absent*, and it is never absent merely because you cannot afford it. The
  loop would run to `maxRadarSweeps` and report `ErrTapCapReached` — a defect
  verdict — on a radar that was simply out of stamina.
- **A currency-spending dialog is now reachable by an automated tap.** This is
  the most consequential finding in this document. It needs a named screen so
  the agent can recognize and leave it, and a rule that the task never
  interacts with it beyond escaping.
- **Replace the sweep loop with a single execute-then-claim pass.** At a
  10800s cadence the next run is three hours away, and one pass per run cannot
  spiral. "Claim All did not appear" becomes an ordinary outcome, not a
  failure, because insufficient stamina is a legitimate reason for it.
- Stamina cannot be read without OCR, which M2 deliberately excludes. So the
  task cannot check affordability in advance — it must tap, detect the prompt,
  and retreat. Recognition is what protects us here, which is invariant #3
  earning its keep.

### New screen: the stamina prompt

Needs an identifying anchor and a `Back` escape edge to `radar`. It is entered
only on transient game state, so it takes **out-edges only** — the third case
now covered by that rule, alongside `alliance_tech_donate`.

---

## 3. Incidental observations

- **The radar screen animates continuously** — a scanner beam sweeps the map.
  Anchors must be cropped from the stable bottom bar and HUD, never the map.
- The radar has an explicit back arrow (bottom-left) and a home button
  (bottom-right) in addition to Android back.
- Mail rewards render as a grid of reward icons over the message list, so the
  list rows are partly obscured during the animation. The bottom button bar is
  not.

---

## 4. Net effect on the vocabulary

Screen count is unchanged at 16, but the membership differs:

| | |
|---|---|
| **added** | `alliance_tech_donate`, `stamina_prompt` |
| **removed** | `rewards_popup` (never existed as a screen) |

---

## 5. Verdicts

1. **`modal`: NOT needed.** Task 6 is deleted, not deferred.
2. **Radar schedule:** VS-days-only still stands — rewards bank in quantity
   (11 observed) — but the sweep loop is replaced by a single pass, and the
   stamina prompt must be modelled before any radar task runs unattended.
