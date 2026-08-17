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

// StoreFrame trusts the caller and never re-screenshots or re-recognizes, so
// it must record whatever image it is handed — even one that would fail
// verification — rather than silently re-deriving one of its own.
func TestStoreFrameRecordsTheExactImageHandedToIt(t *testing.T) {
	tr, err := transport.NewReplayTransportFromImages(runtimetest.Frame("radar"))
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

	held := runtimetest.Frame("mail") // deliberately not what the transport would serve
	id, err := c.StoreFrame(context.Background(), "mail", held)
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 || fc.screenID == nil || *fc.screenID != "mail" {
		t.Fatalf("StoreFrame: id=%d screenID=%v", id, fc.screenID)
	}
	if len(tr.Actions()) != 0 {
		t.Fatalf("StoreFrame touched the transport: %+v", tr.Actions())
	}
	if ids := c.ScreenshotIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ScreenshotIDs: %v", ids)
	}
}

func TestStoreFrameChecksKillSwitchFirst(t *testing.T) {
	ks := &runtimetest.FakeKill{}
	ks.Set(runtime.ErrPaused)
	tr, _ := transport.NewReplayTransportFromImages(runtimetest.Frame("radar"))
	fc := &fakeCapturer{}
	opts := runtimetest.Options(tr, ks)
	opts.Capture = fc
	c, _ := runtime.New(opts)

	if _, err := c.StoreFrame(context.Background(), "radar", runtimetest.Frame("radar")); !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("StoreFrame: got %v, want ErrPaused", err)
	}
	if fc.nextID != 0 {
		t.Fatal("a paused ctx must not record")
	}
}

func TestStoreFrameWithoutCapturerErrors(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	if _, err := c.StoreFrame(context.Background(), "base", runtimetest.Frame("base")); err == nil {
		t.Fatal("StoreFrame with no capturer configured must error")
	}
}

type recordedRun struct {
	accountID int64
	route     string
	frames    []runtime.CaptureFrameRef
	complete  bool
}

type fakeCaptureRecorder struct {
	calls []recordedRun
	err   error
}

func (f *fakeCaptureRecorder) RecordCapture(ctx context.Context, accountID int64, route string, frames []runtime.CaptureFrameRef, complete bool) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, recordedRun{accountID: accountID, route: route, frames: frames, complete: complete})
	return nil
}

func TestRecordCaptureDelegatesToTheConfiguredRecorder(t *testing.T) {
	// newCtx (used elsewhere in this file) wires runtimetest.Options with no
	// CaptureRecorder; RecordCapture needs one, so build against Options
	// directly rather than through that helper.
	fr := &fakeCaptureRecorder{}
	tr, _ := transport.NewReplayTransportFromImages(runtimetest.Frame("base"))
	opts := runtimetest.Options(tr, &runtimetest.FakeKill{})
	opts.CaptureRecorder = fr
	c, err := runtime.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	frames := []runtime.CaptureFrameRef{{ScreenshotID: 1, Seq: 0, OffsetPx: 12, GroupKey: ""}}
	if err := c.RecordCapture(context.Background(), "roster", frames, true); err != nil {
		t.Fatalf("RecordCapture: %v", err)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("got %d RecordCapture calls, want 1", len(fr.calls))
	}
	got := fr.calls[0]
	if got.accountID != 1 || got.route != "roster" || !got.complete || len(got.frames) != 1 {
		t.Fatalf("RecordCapture call = %+v", got)
	}
}

func TestRecordCaptureChecksKillSwitchFirst(t *testing.T) {
	ks := &runtimetest.FakeKill{}
	ks.Set(runtime.ErrPaused)
	tr, _ := transport.NewReplayTransportFromImages(runtimetest.Frame("base"))
	fr := &fakeCaptureRecorder{}
	opts := runtimetest.Options(tr, ks)
	opts.CaptureRecorder = fr
	c, _ := runtime.New(opts)

	if err := c.RecordCapture(context.Background(), "roster", nil, true); !errors.Is(err, runtime.ErrPaused) {
		t.Fatalf("RecordCapture: got %v, want ErrPaused", err)
	}
	if len(fr.calls) != 0 {
		t.Fatal("a paused ctx must not record")
	}
}

func TestRecordCaptureWithoutRecorderErrors(t *testing.T) {
	c, _ := newCtx(t, &runtimetest.FakeKill{}, "base")
	if err := c.RecordCapture(context.Background(), "roster", nil, true); err == nil {
		t.Fatal("RecordCapture with no recorder configured must error")
	}
}
