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

// panicRoute recovers from an unrecognized screen: back ×3, then app
// restart, then wait for the graph's entry screen. Task code never calls or
// sees this — primitives run it internally and surface only ErrLost when
// every rung fails. The run is then marked failed and the agent stops
// rather than flails.
func (c *Ctx) panicRoute(ctx context.Context) (Recognition, error) {
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
	return Recognition{}, fmt.Errorf("runtime: account %d unrecoverable after %d backs and a restart: %w",
		c.accountID, panicBackAttempts, ErrLost)
}
