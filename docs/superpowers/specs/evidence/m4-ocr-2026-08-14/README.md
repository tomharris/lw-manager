# First real ingest: what OCR does to actual frames, 2026-08-14

The first end-to-end ingest of a real capture (capture 1, 62 frames, run 366)
produced **`matched=0 created=0 queued=61`** — every list frame failed at the
group-header parse, so row segmentation never ran at all.

The review queue's raw text is the tell:

```
(es Thisisit CED 4]        <- "This Is It"
[By Footloose SSs—~S       <- "Footloose"
(Bz) Foctioese ges BY      <- "Footloose"
co ~ | SCS | (empty)
```

The group names are *in there*, wrapped in noise. So the crop geometry is
right and something downstream is destroying legibility.

## Finding 1: `vision.Preprocess` is what destroys it

Frame 01 shows the crop the region produces: a clean, crisp
`R4 | This Is It | 2/9 | ▲` bar. Frame 02 shows what the OCR engine is
actually handed after `Preprocess` — `Equalize → AdaptiveThreshold → Invert →
Upscale(3)`. Note the **spurious white bar** across the middle right: adaptive
thresholding has manufactured structure out of the header's flat background
gradient.

Measured on that same crop:

| variant | tesseract output |
|---|---|
| full `Preprocess` (ships today) | `(es Thisisit CED 4]` |
| raw crop, no preprocessing | `RA This Is It an B` |
| **grayscale + upscale only** | **`This Is It an B`** |

Across all 17 stored frames of a real run, the group-name sub-crop with
grayscale+4x reads cleanly on **11 of 17**, against **0 of 61 parseable** for
the shipped chain.

This is the flat-region trap CLAUDE.md already documents for NCC, in a
different algorithm: a normalizing operation applied to a nearly-flat area
amplifies noise into structure. There it made a template match everywhere;
here it makes a threshold invent edges.

## Finding 2: crops must be tight to ONE text line

Frame 05 is a header band that is a few pixels too tall: the header text is
perfectly legible but the next row's top edge bleeds into the bottom, and
PSM 7 (single text line) merges the two into garbage (`Se`). The same effect
turns a VS name crop into `[Yewtarset` when the alliance-tag line below is
included.

The sticky header is *not* pinned to the pixel across every frame — it varies
by a few pixels — so the band needs a margin at the top and a tighter bottom
edge, not just a shift.

## Finding 3: the gate's key field already works

VS ranking, grayscale + 4x, no adaptive threshold:

```
rank    -> 6
points  -> 101,286,241     (exact, separators and all)
```

Points is the field the M4 gate is built around, and it reads perfectly off a
real frame. That is the strongest evidence so far that the pipeline is sound
and the preprocessing is the blocker.

## Finding 4: stylized outlined glyphs do not OCR at all

Frame 03 is the group header's `2/9` member count: grey fill, heavy black
outline, light grey background. Tesseract returns nothing usable from it under
any PSM or charset whitelist tried:

```
plain psm 7            -> "a |;"
digits-only whitelist  -> (empty)
rank badge "R4", psm 8 -> "R"
```

This is not a tuning problem. Outlined game glyphs are a different recognition
problem from anti-aliased UI text, and the rank badge and member count are both
drawn that way. Rank attribution at ingest — reading the group's rank from each
frame's own sticky header — cannot be done by OCR as built.

## Finding 5: `allianceMemberCountRegion` cuts off the value

Its `X2` is `0.60` (x=432), but on the alliance screen the label `Members:`
sits at x≈270–390 and its **value** `97/100` at x≈600–680. The region captures
the label and misses the number, which is why the run warned with
`raw_text="4 ES"`. The constant's own comment already conceded it was
"unverified pending a device session".

## Reproducing

`internal/vision/zz_preproc_probe_test.go` (build tag `scrolldiag`) writes what
the OCR engine is handed for any frame and region, so the chain can be
inspected rather than inferred:

```bash
go test -tags scrolldiag ./internal/vision -run TestPreprocProbe -v -args \
  -ppin <frame.png> -ppout /tmp/out.png -ppy1 0.404 -ppy2 0.438
```

## Finding 4a: the rank badge IS tractable by NCC — measured

Since OCR cannot read the badge, the question is whether template matching can.
Measured on real frames, cropping three candidate regions from the
all-collapsed header stack (evidence frame 06) and cross-correlating the four
ranks against each other:

| crop | size | var(R4) | worst cross-rank NCC |
|---|---|---|---|
| full badge | 46×47 | 4,214,157 | 0.870 |
| text `R4` | 33×22 | 1,763,753 | 0.807 |
| **digit only** | **18×22** | **951,026** | **0.680** |

Digit-only separates best, which is the opposite of the instinct to crop
generously: the shield is identical across ranks, so including it adds
correlation that is shared by every impostor. Its variance is ~2,400 per pixel
(stddev ≈ 49), comfortably clear of the near-degenerate band CLAUDE.md warns
about.

**Cross-capture, which is the only test that means anything** — templates cut
from one capture, probed against sticky headers in a different one:

| probe | best match | score | gap over runner-up |
|---|---|---|---|
| frame 00 | R4 | 1.000 | +0.534 |
| frames 01/03/07/12 | R3 | 0.657 | +0.308 |

The correct rank wins every time, but at **0.657**, not near 1.0. So the
acceptance rule must be **argmax across the templates with a required gap over
the runner-up**, not an absolute score threshold — the same conclusion the
scroll-offset work reached, for the same reason: real variation depresses the
absolute score while leaving separation intact.

Frames 01, 03, 07 and 12 scored identically to three decimals, because the
sticky header is pinned to the same pixels in all of them. That is a third
independent confirmation of the pinning measurement.

**Limitation, stated rather than assumed:** only R3 and R4 have cross-capture
probes here. R1 and R2 were cross-correlated only within a single frame, so
their real-world separation is inferred, not measured. A capture that expands
those groups would settle it.

## Finding 6: `SegmentRows` has no valid signal on real frames

Found after the preprocessing fix removed the header failure that had been
masking it — segmentation had never once run on real pixels.

`SegmentRows` splits rows on scanline mean brightness, assuming the signal is
bimodal: "cards are darker than the page background on both screens, so below
the midpoint is card and above is gap." It sets the split at the midpoint
between the darkest and brightest scanline.

Measured on a real member-list frame (region 0.44–0.89):

```
scanline means: min=149  p10=179  median=200  p90=223  max=246
midpoint used  = 198        <- almost exactly the median
first scanlines: 221 221 220 216 214 205 213 212 209 209 207 200 197 192 ...
```

There are not two populations. Each row contains light card background *and*
dark text, which average together, so the row-mean signal oscillates around 200
with no gap between "card" and "gap". The midpoint therefore lands in the middle
of the noise, and the result is **30 detected bands with a median height of
3px** against a real row pitch of 112.

That also explains the `ErrPitchMismatch` on all 62 frames, and shows the check
doing its job: it refused rather than handing 3px slivers downstream.

Two things follow:

- **This needs a different algorithm, not a tuned threshold.** There is no
  threshold that separates populations that do not exist. The list is strongly
  periodic with a known pitch, so phase-finding (autocorrelation against the
  known pitch, or edge/separator detection in an x-band free of text) is the
  natural approach — measure the offset of a known-period signal rather than
  hunt for a level.
- **`observedPitch` measures the wrong quantity even when bands are found.** It
  is the median band *height* — the card — while `pitch` is the row-to-row
  distance, card plus gap. Those differ by the gap, so the comparison is
  systematically biased even on a healthy frame.
