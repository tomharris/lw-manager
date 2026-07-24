package runtime

import (
	"context"
	"errors"
	"fmt"
)

// maxReplans bounds how many times one NavigateTo call re-plans after
// landing somewhere unexpected. Re-planning absorbs a popup eating a tap;
// unbounded re-planning would mask a broken graph.
const maxReplans = 3

// NavigateTo walks the screen graph to the target screen from wherever the
// device currently is. Landing on an unexpected but recognized screen
// triggers a re-plan from there rather than a failure.
func (c *Ctx) NavigateTo(ctx context.Context, target string) error {
	for attempt := 0; ; attempt++ {
		cur, err := c.CurrentScreen(ctx)
		if err != nil {
			return err
		}
		if cur.Screen == target {
			return nil
		}
		if attempt > maxReplans {
			return fmt.Errorf("runtime: could not reach %q after %d re-plans, stuck on %q", target, maxReplans, cur.Screen)
		}

		path, err := c.graph.Path(cur.Screen, target)
		if err != nil {
			return err
		}
		if err := c.walk(ctx, path); err != nil {
			if errors.Is(err, ErrWrongScreen) {
				continue // recognized, just not where we hoped: re-plan
			}
			return err
		}
	}
}

// walk executes edges until one lands unexpectedly (ErrWrongScreen) or all
// succeed.
func (c *Ctx) walk(ctx context.Context, path []Edge) error {
	for _, e := range path {
		switch e.Action {
		case ActionTap:
			if err := c.Tap(ctx, e.From, e.AnchorID); err != nil {
				return err
			}
		case ActionBack:
			if err := c.ks.Check(ctx); err != nil {
				return err
			}
			if err := c.tr.Back(ctx); err != nil {
				return fmt.Errorf("runtime: back edge %q->%q: %w", e.From, e.To, err)
			}
		default:
			return fmt.Errorf("runtime: edge %q->%q has unknown action %d", e.From, e.To, e.Action)
		}
		if _, err := c.WaitFor(ctx, e.To); err != nil {
			return err
		}
	}
	return nil
}
