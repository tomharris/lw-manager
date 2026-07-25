# Corpus tooling and the M1 gate

## Context

The physical Android phone arrived on 2026-07-25, clearing the blocker
recorded in the emulator-login investigation: Last War fingerprint-blocks
account login on the AVD, so every authenticated screen has been unreachable.
The game is installed and logged in on the handset, which is USB-attached to
the headless Linux box that runs the agent.

Three slices were built device-free while the phone was in transit — the M1
vision core, the M2 task runtime, and the scheduler — and all three are merged
to `main`. Each stopped at the same wall. `fixtures/` is empty, `templates/`
does not exist, and `runtime.DefaultGraph()` names six anchors
(`world_map_button`, `base_button`, `alliance_button`, `tech_button`,
`mail_button`, `radar_button`) that `Validate` refuses to run without. That
refusal is by design: skeleton tasks fail loudly rather than blind-tap
unproven screens.

So the remaining work is not algorithm work. It is calibration against real
pixels, and the M1 gate — screen recognizer ≥ 98% accuracy on the fixture
set, offline, no device attached (design doc line 327).

The non-obvious part: none of the tooling to do that calibration exists.
`agent capture` writes one screenshot to blob and the `screenshots` table,
which is an *observation record*, not a labeled fixture. Nothing crops a
template out of a frame, and nothing scores the recognizer against a labeled
corpus. Building that tooling is most of this spec. It is worth building
rather than doing by hand because a game update invalidates the templates and
the whole process has to run again.

### Scope

In: the capture/label/crop/score tooling, the real corpus, the anchor
templates, and a passing M1 gate.

Out: real Tier 1 task bodies (gather-gold-only, Quick Execute, Claim All),
the M2 24h unattended acceptance run, M4 parsing, live threshold sliders,
studio auth beyond a shared token, multi-device orchestration.

### Scope decisions made during design

| Decision | Choice | Rationale |
|---|---|---|
| Labeling surface | Served web UI | The build host is headless and driven over SSH. A browser on the laptop is the only place a screenshot can be looked at, so labeling *and* cropping must live there |
| Threshold tuning | Batch CLI report, not UI sliders | The number wanted is the one separating true from false positives across 200 frames. A program computes that better than a human drags a slider — which collapses most of the argument for an interactive studio |
| Label storage | The directory *is* the label | No sidecar or index file to desynchronize. Fixable over SSH with `mv`, and it makes `internal/corpus` a thin `Store` over a root directory |
| Frame naming | Content hash, `<sha256>.png` | Free deduplication. A burst capture of an idle base screen collapses to one frame instead of forty skewing the accuracy denominator. The full digest rather than a prefix, so a filename is directly a `blob.Key` input and `push` never has to re-hash to find out where a frame belongs |
| Corpus bytes | Blob store, with a committed `index.yaml` | 200+ full-resolution screenshots is 300–600 MB, which git keeps in history forever. Frames are already content-addressed, so `internal/blob` fits without adaptation |
| Negatives | Part of the gate, not a separate figure | Without them the gate is passable with thresholds so loose every frame matches something, and a misidentified screen is exactly the blind tap invariant #3 forbids |
| Second resolution | Synthetic rescale at score time | Only one handset exists and the emulator cannot reach logged-in screens. Tests the matcher's scale handling; explicitly *not* a claim of cross-device generalization |
| Corpus coverage | Graph screens plus the M4 analytics screens | Capture is human time and the expensive step. Alliance → Members and VS Ranking are needed at M4 anyway, and they stress the recognizer hardest because they share chrome with `alliance` |
| Gate test | `//go:build corpus`, run via `make gate` | Multi-scale NCC over 200+ frames is slow and `make test` must stay fast. Same reasoning that separates the `integration` and `device` tags |

---

## Layout

```
internal/corpus   Store over a root dir: capture, label, enumerate, index
                  Pure filesystem. No device, no DB, no network.
internal/studio   HTTP handlers: label grid, crop view, manifest writer
                  Takes a corpus.Store and a transport.Transport.
internal/vision   score.go       — recognizer scoring over a labeled corpus
                  separation.go  — per-anchor score separation (pure)
                  corpus_test.go — the gate, //go:build corpus
cmd/agent         new: `agent record`  — burst capture into _unsorted/
                       `agent studio`  — the served labeling/cropping UI
                       `agent score`   — the gate harness
                       `agent corpus`  — index | push | pull
fixtures/corpus/  <label>/<sha256>.png working tree; index.yaml committed
templates/        manifest.yaml + anchor PNGs, written by the crop view
```

---

## The corpus

### On-disk shape

```
fixtures/corpus/
  index.yaml            committed to git
  _unsorted/            freshly captured, not yet labeled
  _none/                negatives: must NOT be recognized
  base/
  world_map/
  alliance/
  alliance_tech/
  alliance_members/
  vs_ranking/
  mail/
  radar/
```

The directory tree is the local source of truth for labels. `index.yaml` is
its committed projection, one entry per frame:

```yaml
frames:
  - hash: 3f2a1c8e9b04...
    label: alliance
    width: 1080
    height: 2400
    captured_at: 2026-07-25T14:03:11Z
    device: <model from ro.product.model>
    game_version: <versionName from dumpsys>
```

That metadata belongs in the index precisely *because* the PNGs are not in
git. Without it, a future reader cannot tell whether a corpus predates a game
update, which is the single most likely cause of a gate that used to pass and
now does not.

### Deliberate divergence from the screenshots table

CLAUDE.md records that identical screenshot bytes deduplicate to one blob but
still write a separate `screenshots` row, because each capture is a distinct
observation and collapsing rows would under-report participation.

The corpus inverts that. A duplicate frame here is not an observation, it is
noise, and it would weight the accuracy denominator toward whichever screen
the phone happened to idle on. `agent record` therefore skips byte-identical
frames and reports how many it dropped.

Neither `agent record` nor `agent studio` writes to the `screenshots` table
at all. Fixtures are test assets; mixing them into real capture history would
corrupt the very participation numbers the platform exists to produce.

### Sync

- `agent corpus index` — regenerate `index.yaml` from the directory tree.
- `agent corpus push` — upload frames present in the tree but not in blob.
- `agent corpus pull` — materialize frames named in `index.yaml` into their
  label directories.

The gate test skips when the corpus is not pulled, the same way `test-device`
skips when no device is attached.

---

## `agent record`

Burst capture into `_unsorted/`. Started from the terminal, then the operator
walks away and drives the phone by hand.

```
agent record --account <id> --interval 2s --duration 10m
```

Screenshots go through `transport.Transport.Screenshot`, so `adb exec-out`
handling is already correct. Sleeps go through the jittered context helper
per invariant #7. Byte-identical frames are dropped. Missing device fails
loudly before the first frame rather than producing an empty corpus.

---

## `agent studio`

A small HTTP server on the headless box, reached from the laptop over the
LAN.

```
agent studio --addr 0.0.0.0:8088 --token <secret>
```

The server **refuses to bind a non-loopback address without a token**. When
`--token` is absent it generates one and prints it to stderr. A token supplied
as `?t=` is set as a cookie so subsequent requests carry it.

### Views

**Label grid** — thumbnails of `_unsorted/`, click to assign a label from the
known set or type a new one. Assignment moves the file into the label
directory.

**Labeled browser** — frames per screen, for spotting mislabels and for
picking a good frame to crop from.

**Crop view** — drag a rectangle over a frame, name the anchor, set whether it
identifies the screen, submit.

**Capture now** — take a fresh screenshot from the phone on demand. While
cropping, the wanted screen is easier to produce on the handset than to hunt
for in the corpus.

### Writing a template

`POST /crop` validates that the region is normalized and non-inverted, writes
`templates/<screen>/<anchor_id>.png` and the `manifest.yaml` entry, then
**re-runs `LoadRegistry` and rolls back both writes if the manifest no longer
loads**. `LoadRegistry` already validates loudly — an inverted region, an
out-of-range threshold, a missing template file. Reusing it as a write-time
check means the manifest is never left in a state that breaks
`agent run-task` later.

Crop rectangles arrive from the browser as fractions of the displayed image
and are stored as `transport.Rect`, so invariant #1 holds: the canvas is just
another denormalization boundary, and no absolute pixel coordinate crosses
into the registry.

### Every labeled screen needs an identifying anchor

`DefaultGraph()` navigates six screens, but the corpus labels eight. A frame
labeled `alliance_members` with no identifying anchor in the manifest can
never be recognized correctly, so it counts against the gate on every run.

Therefore: **the manifest gets identifying anchors for all eight labeled
screens**, including `alliance_members` and `vs_ranking`, even though nothing
navigates to them yet. Recognition and navigation are separate concerns —
the recognizer must name every screen the corpus asserts exists. Adding graph
*edges* to those screens is M4 capture-route work and stays out of scope
here.

---

## `agent score` — the gate harness

```
agent score --corpus fixtures/corpus --templates templates/manifest.yaml \
            --gate 0.98 [--rescale 0.75,1.25] [--json] [--apply-thresholds]
```

Walks the labeled corpus, runs `Recognizer.Recognize` on every frame, and
counts a frame correct when the prediction matches its directory label — with
`_none` frames correct only when recognition *fails* with
`ErrNoScreenRecognized`. Exits non-zero below the gate, so it runs in CI.

The headline accuracy is the least useful thing it prints.

### Confusion matrix

Rows are true labels, columns are predictions, with an explicit `<none>`
column. "94% accurate" is not actionable. "Eleven `alliance_tech` frames were
called `alliance`" says the two screens' identifying anchors do not
discriminate. "Eight `_none` frames landed in `radar`" says a threshold is
too loose.

### Per-anchor separation report

For each anchor, score it against *every* frame in the corpus and split the
results into in-screen (frames labeled with that anchor's screen) and
out-of-screen. Report the gap between the worst in-screen score and the best
out-of-screen score.

- **Gap > 0** — a separating threshold exists. Suggest the midpoint and report
  the margin. A wide margin is a robust anchor; a 0.02 margin is one game
  update from breaking.
- **Gap ≤ 0** — the distributions overlap and *no threshold can work*. Flag it
  as a bad anchor.

This is the reason to build the harness rather than hand-tune. Tuning has a
failure mode where numbers get nudged until the aggregate passes, without
anyone noticing that one anchor is non-discriminative and has been papered
over by a loose threshold elsewhere. The separation report makes that
structurally visible, and the two cases call for different *kinds* of action:
gap ≤ 0 means recrop, not retune.

It also interacts with `scoreScreen`'s min-aggregation. A screen scores as its
weakest identifying anchor, so a single bad anchor caps its entire screen.
Per-anchor reporting finds that; per-screen reporting hides it.

### Thresholds are suggested, never applied

Writing suggestions back to the manifest requires an explicit
`--apply-thresholds`. A gate that silently rewrites the manifest to make
itself pass is not a gate.

### Resolution generalization

`--rescale` synthesizes rescaled copies of each frame at score time and
reports their accuracy on a separate line, so scale invariance is measured
without storing a second corpus.

Stated as a limitation in the report itself: synthetic rescaling does not
reproduce a real second device, where a different DPI changes layout and font
hinting rather than only scale. It tests the matcher's scale handling. It is
not evidence of cross-device generalization, and the report must not imply
that it is.

---

## Testing

The load-bearing move is separating *computing* scores from *interpreting*
them.

| Unit | Approach |
|---|---|
| `internal/corpus` | Temp-dir tests: hash naming, dedup on re-capture, label moves, index round-trip |
| `internal/studio` | `httptest` over a temp corpus with `ReplayTransport`: token rejection, crop region validation, and the rollback path when a manifest write would break `LoadRegistry` |
| Score interpretation | Pure functions over `[]{label, predicted}` → confusion matrix, and `[]{anchorID, screen, frameLabel, score}` → separation report. **No images at all**, so the degenerate cases get exhaustive tests: an anchor scoring identically everywhere, a screen with one frame, an empty `_none` set |
| The gate | `internal/vision/corpus_test.go`, `//go:build corpus`, skips when the corpus is not pulled |

Everything except the gate runs under plain `make test` with no device, no
Docker, and no tesseract, per invariant #6.

---

## Capture protocol

The part executed by hand. Roughly 25 frames per screen across the eight
screens, plus about 40 negatives — call it 240 frames.

Variety is what makes a corpus worth having. Capture across different times of
day, with and without notification badges, with mail unread and mail empty,
with alliance tech at different donation states, with and without the alliance
help banner. Negatives mean deliberately wandering into ad overlays, loading
transitions, event popups, and shop screens.

A corpus of 200 near-identical frames proves nothing that 8 frames would not.
The accuracy figure would still read ≥ 98% and would still be worthless.

---

## Sequencing

Ordered so capture starts as early as possible: it is the long pole, and it is
human time rather than compute.

0. Phone onto adb — USB debugging, `agent devices`, `agent register
   --nickname <alt> --role alliance_data`, and one `agent capture` to prove
   the M0 path on real hardware.
1. `internal/corpus` + `agent record`. **Capture begins here**, in parallel
   with everything below.
2. `agent corpus index | push | pull` — captures become durable.
3. Studio label view.
4. Studio crop view and manifest writer.
5. `agent score` with the separation report.
6. Label, crop, tune, `make gate`.

Step 0 is also the first real test of `ADBTransport` against hardware. Every
prior exercise of it has been against the emulator or `ReplayTransport`.

---

## Risks

| Risk | Mitigation |
|---|---|
| Corpus captured against a game version that updates mid-work | `game_version` in `index.yaml`; the separation report's margin column is the early warning |
| Anchors chosen from chrome shared between `alliance` and `alliance_members` | The separation report flags gap ≤ 0 explicitly rather than letting a loose threshold hide it |
| Gate passes on a corpus too uniform to be meaningful | Capture protocol above; the confusion matrix exposes screens with too few frames to say anything |
| Studio exposed on the LAN | Token required for any non-loopback bind, enforced at startup rather than documented |
| One handset means one resolution | Synthetic rescale measured and reported separately, with its limitation stated in the report rather than in a doc nobody rereads |
