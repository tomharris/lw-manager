package corpus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
)

// sliceSource yields canned frames in order, then reports exhaustion. If
// failAfter is positive, it returns failErr (or a generic error) once it has
// yielded that many frames, instead of running out normally — simulating a
// mid-session failure like a dropped USB connection.
type sliceSource struct {
	frames    [][]byte
	i         int
	err       error
	failAfter int
	failErr   error
}

func (s *sliceSource) Frame(context.Context) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.failAfter > 0 && s.i >= s.failAfter {
		if s.failErr != nil {
			return nil, s.failErr
		}
		return nil, errors.New("source failed")
	}
	if s.i >= len(s.frames) {
		return nil, errors.New("source exhausted")
	}
	f := s.frames[s.i]
	s.i++
	return f, nil
}

func noSleep(context.Context) error { return nil }

func TestRecordStoresFramesUnsortedAndCountsRecaptures(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{frames: [][]byte{[]byte("a"), []byte("a"), []byte("b")}}

	res, err := corpus.Record(context.Background(), s, src, corpus.RecordOptions{
		Count: 3,
		Sleep: noSleep,
		Meta:  corpus.Meta{Width: 1080, Height: 2400, Device: "Pixel 8a"},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if res.Captured != 2 {
		t.Fatalf("Captured = %d, want 2", res.Captured)
	}
	if res.Recaptured != 1 {
		t.Fatalf("Recaptured = %d, want 1", res.Recaptured)
	}

	frames, err := s.List(corpus.Unsorted)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("stored %d frames, want 2", len(frames))
	}
}

// The metadata a PNG cannot carry has to come back out of the run so the
// caller can fold it into the index.
func TestRecordReturnsMetaKeyedByHashForCapturedFramesOnly(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{frames: [][]byte{[]byte("a"), []byte("a")}}

	res, err := corpus.Record(context.Background(), s, src, corpus.RecordOptions{
		Count: 2,
		Sleep: noSleep,
		Meta:  corpus.Meta{Device: "Pixel 8a", GameVersion: "3.2.1"},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(res.Metas) != 1 {
		t.Fatalf("Metas = %+v, want one entry", res.Metas)
	}
	for _, m := range res.Metas {
		if m.Device != "Pixel 8a" || m.GameVersion != "3.2.1" {
			t.Fatalf("meta = %+v, want the run's device and game version", m)
		}
	}
}

// Ctrl-C during a ten-minute capture session must keep what it captured.
func TestRecordStopsOnContextCancellationWithoutLosingFrames(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{frames: [][]byte{[]byte("a"), []byte("b"), []byte("c")}}
	ctx, cancel := context.WithCancel(context.Background())

	sleepThenCancel := func(context.Context) error {
		cancel()
		return context.Canceled
	}

	res, err := corpus.Record(ctx, s, src, corpus.RecordOptions{
		Count: 0, // until the context ends
		Sleep: sleepThenCancel,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if res.Captured != 1 {
		t.Fatalf("Captured = %d, want the one frame taken before cancellation", res.Captured)
	}
}

// A USB drop partway through a ten-minute unattended session must not read
// the same as a clean finish: the frames already captured are real and must
// be kept, but the caller has to be told the session ended early rather than
// completing normally.
func TestRecordSurfacesASourceErrorAfterTheFirstFrameWithoutLosingCapturedFrames(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{frames: [][]byte{[]byte("a")}, failAfter: 1, failErr: errors.New("usb disconnected")}

	res, err := corpus.Record(context.Background(), s, src, corpus.RecordOptions{
		Count: 3, // more than the source can actually deliver
		Sleep: noSleep,
	})
	if err == nil {
		t.Fatal("Record returned nil error despite the source failing after the first frame")
	}
	if res.Captured != 1 {
		t.Fatalf("Captured = %d, want the one frame taken before the failure kept", res.Captured)
	}
	frames, listErr := s.List(corpus.Unsorted)
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(frames) != 1 {
		t.Fatalf("stored %d frames, want the one captured frame kept on disk", len(frames))
	}
}

// Context cancellation and deadline expiry are the normal way a session
// ends (Ctrl-C, --duration), and must still exit cleanly even if they
// surface through the source's own Frame error rather than being caught by
// the ctx.Err() check at the top of the loop.
func TestRecordTreatsAContextErrorFromTheSourceAsACleanStop(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{frames: [][]byte{[]byte("a")}, failAfter: 1, failErr: context.Canceled}

	res, err := corpus.Record(context.Background(), s, src, corpus.RecordOptions{
		Count: 3,
		Sleep: noSleep,
	})
	if err != nil {
		t.Fatalf("Record: %v, want nil for a context.Canceled source error after the first frame", err)
	}
	if res.Captured != 1 {
		t.Fatalf("Captured = %d, want 1", res.Captured)
	}
}

func TestRecordFailsLoudlyOnTheFirstFrame(t *testing.T) {
	s := corpus.New(t.TempDir())
	src := &sliceSource{err: errors.New("no device")}

	if _, err := corpus.Record(context.Background(), s, src, corpus.RecordOptions{
		Count: 3,
		Sleep: noSleep,
	}); err == nil {
		t.Fatal("Record succeeded with a failing source")
	}
}
