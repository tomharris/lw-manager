package vision

import (
	"errors"
	"fmt"
	"image"

	"github.com/tomharris/lw-manager/internal/transport"
)

// ErrOffsetUncertain reports that no vertical shift could be measured with
// confidence. It is deliberately distinct from "the list did not move": a
// caller that cannot tell those apart would treat a failed measurement as a
// list bottom and truncate the capture silently.
var ErrOffsetUncertain = errors.New("vision: scroll offset could not be measured")

const (
	// offsetStripFrac is the height of the probe strip as a fraction of the
	// region. Large enough to carry structure, small enough that a full
	// viewport of travel still leaves it inside the previous frame.
	offsetStripFrac = 0.12
	// offsetMinScore is the NCC below which a placement is not believed.
	offsetMinScore = 0.90
	// offsetMinVariance rejects a probe strip flat enough to correlate with
	// anything, which is the same trap that makes a near-flat anchor useless:
	// NCC divides out the template's variance, so a flat strip asks "is this
	// area smooth" and every gap between cards answers yes.
	offsetMinVariance = 50.0
	// offsetProbes is how many candidate strip positions are considered; the
	// highest-variance one wins.
	offsetProbes = 3
)

// ScrollOffset measures how far the content inside region moved up between
// prev and cur, in pixels of the frames' own resolution. Zero means the
// content did not move.
//
// It works by cutting a probe strip from cur near the top of the region and
// finding where that strip sits in prev. Content moving up by d puts a feature
// that was at y+d in prev at y in cur, so the strip is searched downward only.
//
// This is measurement rather than assumption on purpose. Recon on the handset
// found that fling roughly doubles a swipe — a 700px gesture moved ~1504px
// against a ~990px viewport — so a capture that trusts its gesture skips rows
// while every frame still looks valid.
func ScrollOffset(prev, cur image.Image, region transport.Rect) (int, error) {
	if !region.Valid() {
		return 0, fmt.Errorf("vision: scroll region %+v is not a valid unit-square rect", region)
	}
	if prev.Bounds() != cur.Bounds() {
		return 0, fmt.Errorf("vision: frames differ in size: %v vs %v", prev.Bounds(), cur.Bounds())
	}

	h := cur.Bounds().Dy()
	regionTop := int(region.Y1 * float64(h))
	regionBot := int(region.Y2 * float64(h))
	regionH := regionBot - regionTop
	stripH := int(offsetStripFrac * float64(regionH))
	if stripH < 8 || regionH < 4*stripH {
		return 0, fmt.Errorf("vision: region is too short to measure a scroll offset (%d px): %w", regionH, ErrOffsetUncertain)
	}

	strip, stripY, err := bestProbeStrip(cur, region, regionTop, regionH, stripH)
	if err != nil {
		return 0, err
	}

	// Search prev from the strip's own row downward to the region's bottom.
	// Anything above stripY would mean the list scrolled backwards, which this
	// capture loop never does.
	search := transport.Rect{
		X1: region.X1,
		Y1: float64(stripY) / float64(h),
		X2: region.X2,
		Y2: region.Y2,
	}
	res, err := Match(prev, strip, search, h)
	if err != nil {
		return 0, fmt.Errorf("vision: matching the probe strip: %w", err)
	}
	if res.Score < offsetMinScore {
		return 0, fmt.Errorf("vision: best placement scored %.3f, below %.2f: %w", res.Score, offsetMinScore, ErrOffsetUncertain)
	}

	foundY := int(res.Box.Y1 * float64(h))
	d := foundY - stripY
	if d < 0 {
		d = 0
	}
	return d, nil
}

// bestProbeStrip picks the highest-variance candidate strip from cur, and
// refuses to return one flat enough to match anywhere.
func bestProbeStrip(cur image.Image, region transport.Rect, regionTop, regionH, stripH int) (image.Image, int, error) {
	w := cur.Bounds().Dx()
	x0 := int(region.X1 * float64(w))
	x1 := int(region.X2 * float64(w))

	type candidate struct {
		img image.Image
		y   int
		v   float64
	}
	var best candidate
	for i := 0; i < offsetProbes; i++ {
		y := regionTop + (i+1)*stripH
		sub := image.NewRGBA(image.Rect(0, 0, x1-x0, stripH))
		for yy := 0; yy < stripH; yy++ {
			for xx := 0; xx < x1-x0; xx++ {
				sub.Set(xx, yy, cur.At(cur.Bounds().Min.X+x0+xx, cur.Bounds().Min.Y+y+yy))
			}
		}
		v := variance(sub)
		if v > best.v {
			best = candidate{img: sub, y: y, v: v}
		}
	}
	if best.img == nil || best.v < offsetMinVariance {
		return nil, 0, fmt.Errorf("vision: probe strip variance %.1f below %.1f: %w", best.v, offsetMinVariance, ErrOffsetUncertain)
	}
	return best.img, best.y, nil
}
