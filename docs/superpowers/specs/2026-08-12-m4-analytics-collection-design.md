# M4 — Analytics collection: design

**Date:** 2026-08-12
**Milestone:** M4, following M2 (M3 is deferred behind M5)
**Gate:** a daily VS capture matches a hand-checked screenshot within ±1% on
≥95% of rows; every discrepancy lands in review, none silently dropped.

Designed against real frames. `2026-08-12-m4-recon-findings.md` records the
recon pass, which corrected four assumptions this document would otherwise have
been built on. Where a decision here looks over-careful, the findings doc
usually explains why.

M4 is where the architectural thesis gets tested. M0–M2 built a bot that acts;
M4 is the first milestone whose purpose is to observe.

---

## 1. Scope

Two of the design doc's six collection routes:

| route | screen path | yields |
|---|---|---|
| `roster` | Alliance → Members | name, rank, power, level, last-active |
| `vs_ranking` | base → VS → Ranking → Weekly → Your Alliance | per-member VS points |

`vs_ranking` is the gate. `roster` is not optional despite not being named by
the gate: `participation_facts` has a foreign key to `members`, and the fuzzy
matcher has nothing to match against without a roster.

**Out of scope, deliberately:** `alliance_duel`, `gift_chest`, `tech_donation`
and `rally_log`. Once this pipeline exists each is a route definition against
proven machinery, not new machinery. `alliance_duel` is additionally
event-gated and may not be observable when you want to test it, and `rally_log`
has a different shape entirely — event rows, not member rows.

Also out of scope: rank *history* as facts (nothing in M5's metric list needs
it), and the M3 dashboard, which stays deferred.

---

## 2. Architecture

Two commands with a durable queue between them.

```
agent run-task --task roster_capture      (scheduler drives both daily)
agent run-task --task vs_capture
  ├─ navigate via new graph edges, confirming each landing
  ├─ scroll loop: swipe → capture → measure offset → repeat until offset == 0
  └─ writes: captures + capture_frames (screenshot_id, seq, offset_px, group_key)

control ingest
  ├─ for each frame: crop the rows the measured offset says are new
  ├─ preprocess → OCR per field spec → parse
  ├─ fuzzy match against members + aliases
  ├─ ≥92 auto-accept → participation_facts
  │  75–92 → review_queue;  <92 → review_queue, flagged reject
  └─ reconcile → captures.status = complete | partial

agent studio → /review
  └─ resolve → writes member_aliases only; the fact arrives on the next
     `control ingest` pass over the same capture, matched via that alias
```

### Why capture and ingest are separate processes

Three reasons, in order of weight:

1. **Replay.** A parser fix can be re-run over every capture ever taken without
   touching the handset. The expensive, device-bound half is already durable.
2. **Dwell time.** OCR is a `tesseract` subprocess (`CGO_ENABLED=0` leaves no
   in-process option). Running it inline would extend the time the game sits
   open on a ranking screen for no benefit.
3. **Device-free tests.** `internal/ingest` reads fixture blobs. Invariant #6
   holds by construction rather than by discipline.

It also matches the thesis in CLAUDE.md literally: the bot is the collection
tier for the analytics tier.

### Package boundaries

| package | responsibility | new? |
|---|---|---|
| `internal/vision` | `ScrollOffset(prev, cur, region) (int, error)` | extend |
| `internal/tasks` | `roster_capture.go`, `vs_capture.go`, shared scroll helper | extend |
| `internal/ingest` | segmentation, field parsing, fact writing, reconciliation | **new** |
| `internal/roster` | normalization + fuzzy match: `Match(name) → candidates` | **new** |
| `internal/studio` | `/review` views | extend (gains a db dependency) |
| `internal/db` | migration `00005_analytics.sql` | extend |

### Two boundary rules

**The roster route is the only writer of `members`.** A VS row matching nothing
goes to review; it never creates a member. Without this rule one OCR misread
mints a phantom member, that phantom accumulates facts, and it corrupts the
very row count meant to catch the problem. It also fixes ingest ordering:
roster before VS, which is already the natural cadence.

**Ingest never touches a device; capture never touches OCR.** The scroll loop's
stop condition is a measured pixel offset, not a parsed row.

### Why `capture_frames` stores the offset rather than recomputing it

Ingest has the images and could re-measure. It must not. A later change to
`ScrollOffset` would then silently re-segment historical captures into
*different rows*, making old facts unreproducible. Invariant #4 exists so a
number's derivation cannot shift underneath it.

---

## 3. Capture

### Navigation edges

| edge | anchor | status |
|---|---|---|
| `base → alliance` | `alliance_button` | exists |
| `alliance → alliance_members` | `members_button` | **crop** |
| `base → alliance_duel` | `vs_button` | exists (target screen is new) |
| `alliance_duel → vs_ranking` | `ranking_button` | **crop** |
| `vs_ranking → vs_ranking_weekly` | `weekly_tab` | **crop** (exists only on the two screens below) |
| `vs_ranking_weekly → vs_ranking_alliance` | `vs_ranking_alliance_button` | **recrop** |

Return edges are `ActionBack` throughout, matching how the mail and alliance
subtrees are already modelled in `DefaultGraph`.

`alliance_duel` is a seventeenth screen in `ScreenNames`. `base → VS` lands
there, not on a ranking screen, and the route taps through it. Per `screens.go`,
a labeled screen without an identifying anchor is scored wrong on every run
forever, so it owes the corpus frames and an anchor.

The recrop is not cosmetic. `vs_ranking_alliance_button` is the empty checkbox
CLAUDE.md measures at stddev 2,346 — about 11x flatter than a text anchor,
reading `worst-in 1.000 / best-out 1.000`. CLAUDE.md treats that as a scoring
problem. M4 **taps** it, where flatness fails worse: NCC locates its best match
at an arbitrary smooth region and the task taps there while invariant #3
believes an anchor was matched — a blind tap wearing a matched anchor's
clothes. Recrop to include the adjacent "Your Alliance" label so the template
carries text variance.

### The two routes

```
roster_capture:
  navigate base → alliance
  capture the alliance frame            (carries "96/100", tag, name, leader)
  navigate → alliance_members
  for each rank group R4, R3, R2, R1:
      tap the group header
      confirm the chevron flipped to ▼   ← Ctx.Sees, before proceeding
      scroll loop → frames, each carrying the sticky header = the group's rank
      tap to collapse
  R5 comes from the president card, not from a row

vs_capture:
  navigate base → alliance_duel → vs_ranking
  tap Weekly Rank
  tap "Your Alliance"; confirm the checkmark  ← Ctx.Sees
  scroll loop → frames
```

The confirm steps are `startExecution`'s lesson applied to a different verb: a
tapped group header that did not expand yields a perfectly valid capture of the
wrong group.

### The scroll loop

```
capture frame 0
loop:
  swipe 300px over 800ms within the list region     (Ctx.Swipe already jitters)
  capture frame N
  offset := vision.ScrollOffset(prev, cur, listRegion)
  offset == 0        → retry the swipe up to 3 times; still 0 ⇒ bottom, stop
  offset > usable    → rows were never photographed ⇒ mark partial, stop
  else               → record (screenshot_id, seq, offset) and continue
  check the kill switch each iteration               (invariant #8)
bounded by maxFrames = 40; exceeding it ⇒ partial
```

`usable` is the list region's height minus one row pitch — the distance the
list can move while still leaving every row photographed in some frame. The
retry count matches `startExecution`'s three, and for the same reason. 40
frames is roughly double what the largest group needs (R3's 64 members at ~4
rows per swipe is ~16), so exceeding it means something is wrong rather than
merely large.

**Measured on the handset**, ranking screen, 128px rows, ~990px usable region:

| gesture | content moved | result |
|---|---|---|
| 700px / 300ms | ~1504px (~11.6 rows) | **exceeds the viewport — rows skipped** |
| 300px / 800ms | ~512px (~4 rows) | ~48% overlap |

Fling roughly doubles the gesture. The first row is what a naive implementation
writes; it reaches the bottom in 8 swipes and never photographs about a third
of the members while every frame looks valid. So the `offset > usable` check is
not a defensive edge case — it fires on the obvious gesture.

The `offset == 0` retry exists because **a swallowed swipe and a list bottom
produce identical evidence.** A real bottom stays 0 across retries; a swallowed
swipe eventually moves.

`ScrollOffset` takes a region because bottom detection must ignore everything
outside the list. Recon caught a scrolling announcement banner in the header;
while it runs, consecutive frames differ even with the list parked, and
whole-frame comparison would report progress forever.

---

## 4. Ingest

### Row segmentation

Per-frame projection segmentation over the list region: a row-wise intensity
projection locates the card separators, which this UI makes easy. The known row
pitch — 112px roster, 128px ranking — is a **sanity check** on the detected
boundaries rather than the mechanism, so a layout change fails loudly instead
of silently misaligning every field rect.

Two bands are rejected: the row occluded by the sticky group header, and the
pinned self-row band.

### Geometric dedupe

`contentY` accumulates the measured offsets. A row at frame-local `y` sits at
content coordinate `contentY + y`; it is new if that exceeds the last collected
row by more than half a pitch. No OCR is involved, which is the point — OCR
errors perturb identity, and identity-based dedupe would let them perturb the
row count that reconciliation depends on.

The identity check is retained as a **cross-check**, not as the mechanism. It
earns its place: the logged-in account's row is pinned *and* appears in its
natural position, so it is genuinely present twice at two different screen
positions. Geometric dedupe cannot see that; identity can. A disagreement
between the two counts is itself flagged.

### Field specs

| field | charset | notes |
|---|---|---|
| points | `0123456789,` strip `,` | full precision; high `MinConf` |
| power | `0123456789.KMB` | suffix expanded; **4 significant figures is all the game shows** |
| level | `Lv.0123456789` | |
| last-active | `0123456789hmd ago Online` | stored as hours-ago; see below |
| name | unconstrained | lower `MinConf`; the hard one |

Two precision limits are properties of the game, recorded rather than worked
around. Power is abbreviated (`216.2M`), so a weekly delta inherits ±50,000 of
quantization — below the deltas that matter, but stated.

Last-active is displayed relative (`5h ago`), so the fact stores **hours-ago as
its numeric value** and the absolute instant is derivable as
`observed_at − value`. Storing the relative number rather than the derived
timestamp keeps the fact equal to what the screenshot shows, which is what
makes it checkable against the screenshot later. Resolution is about an hour.
`Online` is recorded as `0`.

### Matching

`internal/roster`, hand-rolled rather than vendored — consistent with the
hand-rolled NCC precedent and with `CGO_ENABLED=0`.

Normalization does most of the work: NFKC, strip combining marks, collapse
internal whitespace, casefold. Collapsing whitespace alone fixes
`M I C H E L L`. Then a token-set ratio over Levenshtein.

Thresholds per design doc §5: **≥92 auto-accept, 75–92 review, below reject.**
Every human confirmation writes a `member_aliases` row, so tomorrow's identical
misread matches directly. That compounding is the mechanism that makes accuracy
improve, and it is why the review surface is in M4 rather than deferred.

### Member creation is gated on the group's own count

Names are simultaneously the worst OCR target — `ΔΚΔŽΔ`, `M I C H E L L`,
`Zero^Orca`, `Aureum ⊂👑` — and the field carrying identity. On the roster
route the pipeline *creates* members from them, so a mangled read does not
merely fail to match; it mints a phantom.

The recon supplies a structural guard the design doc lacks: **the group header
states its own total.** If a group says 11 and ingest has already matched 11
existing members, a 12th "new member" in that group is an OCR artifact, not a
person. Creation is therefore gated on the group count, not on a confidence
threshold alone — a structural check where the alternative is a tuned number.

### Confidence

A fact's confidence is `min(ocr_confidence, normalized_match_score)`, with both
stored. A name matched at 0.95 whose points read at 0.6 is not a 0.95 fact.
Gate at 0.80 per design doc §5; below it the row goes to review and never to a
leaderboard (invariant #5).

### Review, in studio

`/review` renders the row crop from the blob beside the ranked candidates —
which is the whole reason it is a served UI rather than a CLI: the box is
headless, and a review without the pixels is not a review. Studio already
serves `GET /frame/{hash}` and already solved browser-over-SSH for corpus
labeling.

Resolving cannot write the fact directly, only the alias. By the time a name
fails to resolve, ingest has already read that row's numeric fields, scored
them, and discarded them — matching happens before a row has anywhere to put
a value, not after. So resolving a review row writes a `member_aliases` row
and nothing else; the fact itself arrives the next time `control ingest` runs
over the same capture, which now matches that row via the alias just written
and stamps it with the capture's own `period_key` and `observed_at`, the same
as any row that resolved the first time.

The alternative — storing the parsed value on the `review_queue` row so
resolution could write the fact immediately — was rejected. It would leave a
second copy of the number living outside `participation_facts`, with the
eventual fact built from that copy rather than re-derived from the pixels on
re-ingest. That is weaker provenance than every other fact in the system
carries, and provenance is the property invariant #4 exists to protect: every
number traces back to a screenshot, not to a value cached the last time
someone looked at one.

This grows studio a database dependency it does not currently have. Accepted:
the alternative is a second server that duplicates its auth, templates and
blob-serving.

---

## 5. Schema

Migration `00005_analytics.sql`:

```sql
alliances(id, tag, name, server, member_count, observed_at)
members(id, alliance_id, name, name_normalized, rank,
        first_seen_at, left_at, active)
member_aliases(id, member_id, alias, alias_normalized, source, created_at)

captures(id, account_id, route, started_at, ended_at, status,
         expected_rows, parsed_rows, error)
capture_frames(id, capture_id, seq, screenshot_id, offset_px, group_key)

participation_facts(...)   -- design doc §5 verbatim: append-only,
                           -- superseded_by, screenshot_id, confidence,
                           -- period_key, source, UNIQUE(member_id, metric,
                           -- period_key, source, observed_at)
review_queue(id, capture_id, screenshot_id, row_rect, raw_text,
             candidates_json, status, resolved_by, resolved_at)
```

`capture_frames.group_key` carries the rank group, because on the roster route
the rank belongs to the frame's sticky header rather than to any row.

`captures.status` is `running | complete | partial | failed`. Members are
soft-deleted via `left_at`, never removed.

Facts written by M4: `power`, `level`, `last_active_hours` daily per member;
`vs_points` weekly, `period_key` = `2026-W33`.

---

## 6. Reconciliation

### Roster: per group, then in total

Each group header states its expected total, and the totals sum to the alliance
member count:

```
R5 1  +  R4 9  +  R3 64  +  R2 11  +  R1 11  =  96      = "Members: 96/100"
```

Parsed rows are checked per group first, then summed against the count read
from the `alliance` frame. Any mismatch marks the capture `partial`, and
`partial` captures are excluded from derived metrics.

Per-group checking is strictly better than one global number: a shortfall
localizes to one group instead of leaving a single discrepancy to explain.

### VS: absence means zero, but only on a complete capture

**The weekly ranking lists only members with a nonzero score.** Recon measured
94 ranked rows against 96 members, and on the daily tab the logged-in account
read `Unlisted / 0 points`. The milestone was scoped on the ranking showing
everyone; it does not, so reconciliation cannot be an equality check.

> A member absent from a **complete** weekly capture is recorded as zero. On a
> `partial` capture no zeros are written at all, because absence and truncation
> are indistinguishable there.

This promotes bottom-of-list proof from a completeness nicety to a correctness
precondition, and it is why the `offset == 0` retry is not optional.

---

## 7. The gate

`make gate-m4`, following the M1 pattern: build-tagged (`m4gate`), device-free,
skipped when fixtures have not been pulled, carrying an explicit `-timeout`.

One real VS capture's frames live in the blob store. A committed hand-checked
YAML lists the expected rows. The test runs ingest over the frames and asserts:

1. parsed value within ±1% of the hand-checked value on ≥95% of rows;
2. every discrepancy produced a `review_queue` row — none dropped;
3. the capture reconciles to `complete`.

Condition 2 is the one that actually distinguishes this gate from an accuracy
number. A pipeline that silently drops its hard rows scores well on condition 1.

---

## 8. Testing

| layer | tag | covers |
|---|---|---|
| unit | none | `ScrollOffset`, segmentation, field parsing, normalization, matching |
| integration | `integration` | fact writes, supersession, review resolution → `lw_manager_test` |
| device | `device` | both capture routes against real adb |
| gate | `m4gate` | ingest vs the hand-checked capture |

`go test ./...` must still pass with no emulator, no adb and no Docker.

The seven recon frames in `evidence/m4-recon-2026-08-12/` are usable
segmentation and parsing fixtures as they stand: they cover the sticky-header
occlusion, the pinned self-row, both row pitches, and the decorated names.

New packages get a fake or a replay path before a real implementation, per
CLAUDE.md. `internal/ingest` is written against `ocr.FakeEngine` first.

---

## 9. Documentation owed

- **CLAUDE.md Layout** — `internal/ingest`, `internal/roster`.
- **CLAUDE.md operational reality** — the `stayon` claim. Recon found the
  handset locked with `mStayOn=true`, `stay_on_while_plugged_in=15`, USB
  powered and no reboot in 13 days, which is a counterexample to calling
  `stayon` "the load-bearing one" on this device.
- **CLAUDE.md gotchas** — a `FLAG_SECURE` surface returns a *zero-byte*
  capture, not a black frame, and fails at PNG decode rather than at
  recognition. A black frame is also what a transition looks like, which
  weakens the inference Phase B drew from that signal.
- **`ScreenNames`** — `alliance_duel`, with corpus frames and an anchor, and an
  M1 gate re-run at seventeen screens.

---

## 10. Risks

| risk | mitigation |
|---|---|
| Name OCR is too poor to identify members at all | Normalization before matching; review queue compounds aliases; member creation gated on group counts. If auto-accept rates are unusable after the first real capture, that is a finding to act on, not to tune around silently. |
| A game update moves the rows | `index.yaml` carries `game_version`; a 39.7MB update landed during recon, so this is live, not theoretical |
| Scroll parameters differ on another device | Offsets are measured, not assumed; the parameters are a starting point and the `offset > usable` check catches a bad one |
| Ingest ordering violated (VS before roster) | Roster is the only writer of `members`; an unmatched VS row reviews rather than creates |
