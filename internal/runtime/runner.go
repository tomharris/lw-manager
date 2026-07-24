package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TaskFunc is a task: plain imperative Go over the constrained Ctx.
type TaskFunc func(ctx context.Context, rt *Ctx) error

// RunStore is the slice of the database the runner needs. *db.Pool
// satisfies it.
type RunStore interface {
	StartTaskRun(ctx context.Context, accountID int64, taskName string) (int64, error)
	FinishTaskRun(ctx context.Context, id int64, status string, errMsg *string, screenshotIDs []int64) error
}

// Outcome is what a completed run looked like.
type Outcome struct {
	RunID  int64
	Status string
}

// AccountID reports which account this Ctx drives.
func (c *Ctx) AccountID() int64 { return c.accountID }

// Run executes one task under a task_runs record. The running row is
// written before the task acts (a killed process leaves evidence), and the
// finishing update uses a detached context so ctrl-C mid-task still gets
// its outcome recorded.
func Run(ctx context.Context, store RunStore, rt *Ctx, name string, fn TaskFunc) (Outcome, error) {
	runID, err := store.StartTaskRun(ctx, rt.accountID, name)
	if err != nil {
		return Outcome{}, fmt.Errorf("runtime: starting run of %q for account %d: %w", name, rt.accountID, err)
	}

	taskErr := fn(ctx, rt)

	status := "succeeded"
	var msg *string
	switch {
	case taskErr == nil:
	case errors.Is(taskErr, ErrPaused), errors.Is(taskErr, ErrAccountDisabled):
		status = "paused"
		s := taskErr.Error()
		msg = &s
	default:
		status = "failed"
		s := taskErr.Error()
		msg = &s
	}

	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if ferr := store.FinishTaskRun(finishCtx, runID, status, msg, rt.ScreenshotIDs()); ferr != nil {
		if taskErr != nil {
			return Outcome{RunID: runID, Status: status}, errors.Join(taskErr, ferr)
		}
		return Outcome{RunID: runID, Status: status}, ferr
	}
	return Outcome{RunID: runID, Status: status}, taskErr
}
