package runtime

import (
	"context"
	"errors"
	"fmt"
	"image"
	"time"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// verifyScreen proves the device currently shows the named screen and
// returns the frame that proves it. Unrecognized frames go through the
// panic route once; a recognized-but-different screen is ErrWrongScreen.
func (c *Ctx) verifyScreen(ctx context.Context, screen string) (image.Image, error) {
	r, frame, err := c.recognize(ctx)
	if errors.Is(err, vision.ErrNoScreenRecognized) {
		if _, rerr := c.panicRoute(ctx); rerr != nil {
			return nil, rerr
		}
		r, frame, err = c.recognize(ctx)
	}
	if err != nil {
		return nil, err
	}
	if r.Screen != screen {
		return nil, fmt.Errorf("runtime: on %q, want %q: %w", r.Screen, screen, ErrWrongScreen)
	}
	return frame, nil
}

// Tap verifies the screen, matches the anchor, and taps a jittered point
// inside the match's bounding box. It accepts no coordinates: invariant #3
// (no blind taps) is enforced by this signature.
func (c *Ctx) Tap(ctx context.Context, screen, anchorID string) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	s, ok := c.reg.Screen(screen)
	if !ok {
		return fmt.Errorf("runtime: unknown screen %q", screen)
	}
	var anchor *vision.Anchor
	for i := range s.Anchors {
		if s.Anchors[i].ID == anchorID {
			anchor = &s.Anchors[i]
			break
		}
	}
	if anchor == nil {
		return fmt.Errorf("runtime: screen %q has no anchor %q", screen, anchorID)
	}

	frame, err := c.verifyScreen(ctx, screen)
	if err != nil {
		return err
	}
	m, err := vision.Match(frame, anchor.Template, anchor.Region, c.reg.ReferenceHeight)
	if err != nil {
		return fmt.Errorf("runtime: matching %q on %q: %w", anchorID, screen, err)
	}
	if m.Score < anchor.Threshold {
		return fmt.Errorf("runtime: screen %q anchor %q scored %.3f below %.3f: %w",
			screen, anchorID, m.Score, anchor.Threshold, ErrAnchorNotFound)
	}
	return c.tr.Tap(ctx, c.jitterPoint(m.Box))
}

// jitterPoint picks a point inside the central 60% of a matched box — never
// dead center, never the edges. Fixed tap pixels are as detectable as fixed
// timing.
func (c *Ctx) jitterPoint(box transport.Rect) transport.Norm {
	fx := 0.2 + 0.6*c.rand.Float64()
	fy := 0.2 + 0.6*c.rand.Float64()
	return transport.Norm{
		X: box.X1 + (box.X2-box.X1)*fx,
		Y: box.Y1 + (box.Y2-box.Y1)*fy,
	}
}

// Swipe drags between two normalized points with jittered endpoints and
// duration. Endpoints are positional (scrolling has no anchor), but the
// screen must still be recognized first: invariant #3 covers every action.
func (c *Ctx) Swipe(ctx context.Context, from, to transport.Norm) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	if _, err := c.recognizeOrRecover(ctx); err != nil {
		return err
	}
	d := jitter(c.rand, 250*time.Millisecond, 600*time.Millisecond)
	return c.tr.Swipe(ctx, c.jitterNorm(from), c.jitterNorm(to), d)
}

// jitterNorm nudges a point by up to ±2% per axis, clamped to the unit
// square so jitter can never manufacture an out-of-range coordinate.
func (c *Ctx) jitterNorm(p transport.Norm) transport.Norm {
	return transport.Norm{
		X: clamp01(p.X + (c.rand.Float64()-0.5)*0.04),
		Y: clamp01(p.Y + (c.rand.Float64()-0.5)*0.04),
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// TypeText types into the focused field, requiring a recognized screen.
func (c *Ctx) TypeText(ctx context.Context, s string) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	if _, err := c.recognizeOrRecover(ctx); err != nil {
		return err
	}
	return c.tr.TypeText(ctx, s)
}
