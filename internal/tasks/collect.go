package tasks

import (
	"context"
	"errors"

	"github.com/tomharris/lw-manager/internal/runtime"
)

// collectTask builds the shared Tier 1 shape: navigate to a screen, tap a
// collect-style button if it is present. A missing anchor is "nothing to
// collect", not a failure — the help-all button simply is not rendered when
// no one needs help.
//
// Each task keeps its own file even while they are this similar: real
// corpus screens will differentiate them (scrolling, multi-tap, sub-menus).
func collectTask(screen, anchorID string) runtime.TaskFunc {
	return func(ctx context.Context, rt *runtime.Ctx) error {
		if err := rt.NavigateTo(ctx, screen); err != nil {
			return err
		}
		if err := rt.Tap(ctx, screen, anchorID); err != nil {
			if errors.Is(err, runtime.ErrAnchorNotFound) {
				return nil
			}
			return err
		}
		return nil
	}
}
