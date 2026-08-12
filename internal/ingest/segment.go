// Package ingest turns stored capture frames into facts. It never touches a
// device: everything here reads decoded images and writes rows, which is what
// keeps its tests device-free and lets a parser fix be replayed over every
// capture ever taken.
package ingest

import (
	"errors"
	"fmt"
	"image"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// ErrPitchMismatch reports that the detected rows do not have the expected
// height. It is a layout-change alarm: silently misaligned rows would keep
// producing plausible numbers from the wrong pixels.
var ErrPitchMismatch = errors.New("ingest: detected rows do not match the expected pitch")

// pitchTolerance is how far a detected band may sit from the expected pitch
// before segmentation is disbelieved, as a fraction of the pitch.
const pitchTolerance = 0.25

// RowBand is one detected row, in pixel coordinates of the frame it came from.
type RowBand struct {
	Y0, Y1 int
}

// Height returns the band's height in pixels.
func (b RowBand) Height() int { return b.Y1 - b.Y0 }

// SegmentRows splits the list region into row bands by projecting brightness
// across each scanline and cutting at the light gaps between cards.
//
// pitch is the expected row height and is checked rather than assumed: a
// detected band more than pitchTolerance away from it means the layout moved,
// which must fail loudly. Recon measured 112px on the roster and 128px on the
// ranking.
func SegmentRows(img image.Image, region transport.Rect, pitch int) ([]RowBand, error) {
	if !region.Valid() {
		return nil, fmt.Errorf("ingest: list region %+v is not a valid unit-square rect", region)
	}
	if pitch <= 0 {
		return nil, fmt.Errorf("ingest: pitch must be positive, got %d", pitch)
	}

	g := vision.Grayscale(img)
	b := g.Bounds()
	top := b.Min.Y + int(region.Y1*float64(b.Dy()))
	bot := b.Min.Y + int(region.Y2*float64(b.Dy()))
	x0 := b.Min.X + int(region.X1*float64(b.Dx()))
	x1 := b.Min.X + int(region.X2*float64(b.Dx()))
	if bot-top < pitch/2 {
		return nil, nil
	}

	// Row-mean brightness across the region.
	means := make([]float64, 0, bot-top)
	for y := top; y < bot; y++ {
		var sum float64
		for x := x0; x < x1; x++ {
			sum += float64(g.GrayAt(x, y).Y)
		}
		means = append(means, sum/float64(x1-x0))
	}

	// Split at the midpoint between the darkest and brightest scanline. Cards
	// are darker than the page background on both screens, so "below the
	// midpoint" is card and "above" is gap. A midpoint is used rather than a
	// fixed constant so the same code works on either screen's palette.
	lo, hi := means[0], means[0]
	for _, m := range means {
		if m < lo {
			lo = m
		}
		if m > hi {
			hi = m
		}
	}
	if hi-lo < 8 {
		// The region is essentially uniform: no cards to find.
		return nil, nil
	}
	mid := (lo + hi) / 2

	var bands []RowBand
	inCard := false
	start := 0
	for i, m := range means {
		switch {
		case m < mid && !inCard:
			inCard, start = true, top+i
		case m >= mid && inCard:
			inCard = false
			bands = append(bands, RowBand{Y0: start, Y1: top + i})
		}
	}
	if inCard {
		bands = append(bands, RowBand{Y0: start, Y1: bot})
	}

	// Drop slivers: partial rows clipped by the region edge are not parseable
	// and must not be handed downstream as if they were whole.
	whole := bands[:0]
	var droppedBands []RowBand
	for _, band := range bands {
		if float64(band.Height()) >= float64(pitch)*(1-pitchTolerance) {
			whole = append(whole, band)
		} else {
			droppedBands = append(droppedBands, band)
		}
	}
	bands = whole

	// If all bands were dropped as slivers, check if it's due to pitch mismatch.
	// Slivers clipped by the region edge have inconsistent sizes; a pitch
	// mismatch means all detected bands are consistently far from the expected
	// pitch, which is a layout change, not a clipping artifact.
	if len(bands) == 0 && len(droppedBands) > 0 {
		for _, band := range droppedBands {
			delta := float64(band.Height()-pitch) / float64(pitch)
			if delta > pitchTolerance || delta < -pitchTolerance {
				return nil, fmt.Errorf("ingest: band %+v is %d px against an expected pitch of %d: %w", band, band.Height(), pitch, ErrPitchMismatch)
			}
		}
	}

	for _, band := range bands {
		delta := float64(band.Height()-pitch) / float64(pitch)
		if delta > pitchTolerance || delta < -pitchTolerance {
			return nil, fmt.Errorf("ingest: band %+v is %d px against an expected pitch of %d: %w", band, band.Height(), pitch, ErrPitchMismatch)
		}
	}
	return bands, nil
}
