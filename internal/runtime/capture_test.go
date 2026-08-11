package runtime_test

import (
	"context"
	"errors"
	"image"
	"testing"

	"github.com/tomharris/lw-manager/internal/capture"
	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/transport"
)

type capturedFrame struct {
	accountID int64
	screenID  *string
}

type fakeCapturer struct {
	nextID   int64
	screenID *string
	calls    []capturedFrame
	err      error
}

func (f *fakeCapturer) Record(ctx context.Context, accountID int64, img image.Image, screenID *string) (capture.Result, error) {
	if f.err != nil {
		return capture.Result{}, f.err
	}
	f.nextID++
	f.screenID = screenID
	f.calls = append(f.calls, capturedFrame{accountID: accountID, screenID: screenID})
	return capture.Result{ScreenshotID: f.nextID, AccountID: accountID}, nil
}

func TestCaptureVerifiesScreenAndRecords(t *testing.T) {
	tr, err := transport.NewReplayTransportFromImages(runtimetest.Frames("radar", "radar")...)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeCapturer{}
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Capture = fc
	c, err := runtime.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	id, err := c.Capture(context.Background(), "radar")
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 || fc.screenID == nil || *fc.screenID != "radar" {
		t.Fatalf("capture: id=%d screenID=%v", id, fc.screenID)
	}
	if ids := c.ScreenshotIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ScreenshotIDs: %v", ids)
	}
}

func TestCaptureRefusesWrongScreen(t *testing.T) {
	tr, _ := transport.NewReplayTransportFromImages(runtimetest.Frame("base"))
	fc := &fakeCapturer{}
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.Capture = fc
	c, _ := runtime.New(opts)

	if _, err := c.Capture(context.Background(), "radar"); !errors.Is(err, runtime.ErrWrongScreen) {
		t.Fatalf("got %v, want ErrWrongScreen", err)
	}
	if fc.nextID != 0 {
		t.Fatal("recorded a frame from the wrong screen")
	}
}

func TestCaptureWithoutCapturerErrors(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	if _, err := c.Capture(context.Background(), "base"); err == nil {
		t.Fatal("Capture with no capturer configured must error")
	}
}
