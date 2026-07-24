package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
)

type fakeRunStore struct {
	started  []string
	finished map[int64]string // id → status
	errMsg   *string
}

func (f *fakeRunStore) StartTaskRun(ctx context.Context, accountID int64, taskName string) (int64, error) {
	f.started = append(f.started, taskName)
	return int64(len(f.started)), nil
}

func (f *fakeRunStore) FinishTaskRun(ctx context.Context, id int64, status string, errMsg *string, screenshotIDs []int64) error {
	if f.finished == nil {
		f.finished = map[int64]string{}
	}
	f.finished[id] = status
	f.errMsg = errMsg
	return nil
}

func runWith(t *testing.T, fn runtime.TaskFunc) (*fakeRunStore, runtime.Outcome, error) {
	t.Helper()
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	st := &fakeRunStore{}
	out, err := runtime.Run(context.Background(), st, c, "test_task", fn)
	return st, out, err
}

func TestRunRecordsSuccess(t *testing.T) {
	st, out, err := runWith(t, func(ctx context.Context, rt *runtime.Ctx) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "succeeded" || st.finished[out.RunID] != "succeeded" {
		t.Fatalf("outcome %+v, store %+v", out, st.finished)
	}
	if st.errMsg != nil {
		t.Fatalf("error message on success: %q", *st.errMsg)
	}
}

func TestRunRecordsFailure(t *testing.T) {
	boom := errors.New("boom")
	st, out, err := runWith(t, func(ctx context.Context, rt *runtime.Ctx) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("task error not returned: %v", err)
	}
	if out.Status != "failed" || st.finished[out.RunID] != "failed" {
		t.Fatalf("outcome %+v", out)
	}
	if st.errMsg == nil || *st.errMsg != "boom" {
		t.Fatalf("errMsg: %v", st.errMsg)
	}
}

func TestRunRecordsPauseDistinctly(t *testing.T) {
	_, out, err := runWith(t, func(ctx context.Context, rt *runtime.Ctx) error {
		return runtime.ErrPaused
	})
	if !errors.Is(err, runtime.ErrPaused) {
		t.Fatal(err)
	}
	if out.Status != "paused" {
		t.Fatalf("status: got %q, want paused — a pause is not a failure", out.Status)
	}
}

func TestRunFinishesEvenWhenContextCancelled(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	st := &fakeRunStore{}
	ctx, cancel := context.WithCancel(context.Background())
	out, err := runtime.Run(ctx, st, c, "test_task", func(ctx context.Context, rt *runtime.Ctx) error {
		cancel() // the operator hits ctrl-C mid-task
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("expected the cancellation to surface")
	}
	if st.finished[out.RunID] != "failed" {
		t.Fatalf("cancelled run not finished in store: %+v", st.finished)
	}
}
