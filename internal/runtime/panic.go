package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomharris/lw-manager/internal/vision"
)

// panicBackAttempts is how many back presses to try before restarting the
// app. Popups and interstitials die to back; deeper corruption does not.
const panicBackAttempts = 3

// LostScreenID labels the frame the panic route blobs when it gives up. It is
// deliberately a sentinel rather than NULL: screenshots.screen_id is free text
// with no foreign key, so a marker costs nothing and makes the evidence
// findable with `where screen_id = '_lost'`. A NULL is indistinguishable from
// every other capture that simply had no screen attached.
const LostScreenID = "_lost"

// panicRoute recovers from an unrecognized screen: back ×3, then app
// restart, then wait for the graph's entry screen. Task code never calls or
// sees this — primitives run it internally and surface only ErrLost when
// every rung fails. The run is then marked failed and the agent stops
// rather than flails.
func (c *Ctx) panicRoute(ctx context.Context) (Recognition, error) {
	// Rung zero: the display may simply be asleep. That is the cheapest cause
	// of total recognition failure and the only one the rungs below cannot
	// touch — a black frame matches no anchor, and neither a back press nor an
	// app restart turns a screen on. Trying it first also keeps a doze from
	// spending three blind back presses on a screen nobody can see.
	if err := c.ks.Check(ctx); err != nil {
		return Recognition{}, err
	}
	c.log.Warn("panic route: waking the display")
	if err := c.tr.Wake(ctx); err != nil {
		return Recognition{}, fmt.Errorf("runtime: panic route wake: %w", err)
	}
	if err := c.Sleep(ctx, c.poll, 2*c.poll); err != nil {
		return Recognition{}, err
	}
	r, _, err := c.recognize(ctx)
	if err == nil {
		c.log.Info("panic route: recovered by waking the display", "screen", r.Screen)
		return r, nil
	}
	if !errors.Is(err, vision.ErrNoScreenRecognized) {
		return Recognition{}, err
	}

	for i := 1; i <= panicBackAttempts; i++ {
		if err := c.ks.Check(ctx); err != nil {
			return Recognition{}, err
		}
		c.log.Warn("panic route: pressing back", "attempt", i)
		if err := c.tr.Back(ctx); err != nil {
			return Recognition{}, fmt.Errorf("runtime: panic route back press: %w", err)
		}
		if err := c.Sleep(ctx, c.poll, 2*c.poll); err != nil {
			return Recognition{}, err
		}
		r, _, err := c.recognize(ctx)
		if err == nil {
			c.log.Info("panic route: recovered", "screen", r.Screen, "backs", i)
			return r, nil
		}
		if !errors.Is(err, vision.ErrNoScreenRecognized) {
			return Recognition{}, err
		}
	}

	c.log.Warn("panic route: back exhausted, restarting app")
	if err := c.tr.AppRestart(ctx); err != nil {
		return Recognition{}, fmt.Errorf("runtime: panic route restart: %w", err)
	}
	deadline := time.Now().Add(c.restartTimeout)
	for time.Now().Before(deadline) {
		if err := c.ks.Check(ctx); err != nil {
			return Recognition{}, err
		}
		if err := c.Sleep(ctx, c.poll, 2*c.poll); err != nil {
			return Recognition{}, err
		}
		r, _, err := c.recognize(ctx)
		if err == nil && r.Screen == c.graph.Entry {
			c.log.Info("panic route: recovered via restart")
			return r, nil
		}
		if err != nil && !errors.Is(err, vision.ErrNoScreenRecognized) {
			return Recognition{}, err
		}
	}
	c.recordLostFrame(ctx)
	return Recognition{}, fmt.Errorf("runtime: account %d unrecoverable after %d backs and a restart: %w",
		c.accountID, panicBackAttempts, ErrLost)
}

// lostCaptureTimeout bounds the evidence capture. Generous relative to a
// screencap, because this path runs while the device is already misbehaving,
// but bounded: a hung blob store must not hold the route open.
const lostCaptureTimeout = 15 * time.Second

// recordLostFrame blobs the frame the route is about to give up on, so a
// recognition failure leaves something to look at.
//
// Best effort by construction — every failure here is logged and swallowed,
// because the caller's ErrLost is the diagnosis and evidence must never
// replace it. It runs on a detached context for the same reason runner.go
// detaches its finish path: this frame is most valuable exactly when the run
// is being torn down.
//
// The id joins ScreenshotIDs, so the failed task_runs row points at the
// evidence instead of merely describing it.
func (c *Ctx) recordLostFrame(ctx context.Context) {
	if c.cap == nil {
		c.log.Warn("panic route: giving up with no capturer configured, so no frame is retained")
		return
	}
	capCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lostCaptureTimeout)
	defer cancel()

	frame, err := c.tr.Screenshot(capCtx)
	if err != nil {
		c.log.Warn("panic route: could not screenshot the lost frame", "error", err)
		return
	}
	screenID := LostScreenID
	res, err := c.cap.Record(capCtx, c.accountID, frame, &screenID)
	if err != nil {
		c.log.Warn("panic route: could not record the lost frame", "error", err)
		return
	}
	c.screenshotIDs = append(c.screenshotIDs, res.ScreenshotID)
	c.log.Warn("panic route: gave up, frame retained",
		"screenshot_id", res.ScreenshotID, "screen_id", LostScreenID)
}
