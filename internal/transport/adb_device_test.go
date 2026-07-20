//go:build device

// Tests in this file need a real device attached. They are tagged `device`
// rather than `integration` because they need different infrastructure: an
// emulator or handset on adb, not Docker. Keeping the tags separate means
// `make test-integration` stays runnable on a machine with no device.
//
//	make test-device
package transport

import (
	"context"
	"errors"
	"image"
	"os"
	"testing"
	"time"
)

func deviceTransport(t *testing.T) *ADBTransport {
	t.Helper()
	ctx := context.Background()

	serial := os.Getenv("LW_DEVICE_SERIAL")
	if serial == "" {
		serials, err := ListDevices(ctx, "adb")
		if err != nil {
			t.Skipf("adb unavailable: %v", err)
		}
		if len(serials) != 1 {
			t.Skipf("want exactly one attached device, got %d; set LW_DEVICE_SERIAL", len(serials))
		}
		serial = serials[0]
	}

	tr, err := NewADBTransport(ctx, ADBOptions{Serial: serial})
	if err != nil {
		t.Fatalf("NewADBTransport(): %v", err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

// The resolution probe is the first thing every session does, and a wrong
// answer silently mislocates every subsequent tap.
func TestDeviceResolutionProbe(t *testing.T) {
	tr := deviceTransport(t)
	size := tr.Resolution()
	if size.X <= 0 || size.Y <= 0 {
		t.Fatalf("resolution = %v, want positive dimensions", size)
	}
	if size.X > size.Y {
		t.Logf("device is landscape (%dx%d); the template library assumes portrait", size.X, size.Y)
	}
}

// Screenshot must survive the round trip through `exec-out screencap -p`.
// This is the case `adb shell` breaks: CRLF translation corrupts the PNG
// while still exiting 0, so only a real decode catches it.
func TestDeviceScreenshotDecodes(t *testing.T) {
	tr := deviceTransport(t)
	img, err := tr.Screenshot(context.Background())
	if err != nil {
		t.Fatalf("Screenshot(): %v", err)
	}
	b := img.Bounds()
	if got, want := b.Dx(), tr.Resolution().X; got != want {
		t.Errorf("screenshot width = %d, want %d", got, want)
	}
	if got, want := b.Dy(), tr.Resolution().Y; got != want {
		t.Errorf("screenshot height = %d, want %d", got, want)
	}
}

// Out-of-range coordinates must be refused rather than clamped: a clamped tap
// lands somewhere real and does something unintended, which is far worse than
// a failed task.
func TestDeviceRejectsOutOfRangeNorm(t *testing.T) {
	tr := deviceTransport(t)
	ctx := context.Background()
	for _, p := range []Norm{{X: 1.5, Y: 0.5}, {X: 0.5, Y: -0.1}} {
		err := tr.Tap(ctx, p)
		var oor ErrOutOfRange
		if !errors.As(err, &oor) {
			t.Errorf("Tap(%v) error = %v, want ErrOutOfRange", p, err)
			continue
		}
		// The offending point must survive: it is the only clue an operator
		// gets about which upstream calculation went wrong.
		if oor.Point != p {
			t.Errorf("ErrOutOfRange.Point = %v, want %v", oor.Point, p)
		}
	}
}

// A normalized gesture must actually move the device. A swipe up from the
// bottom edge opens the launcher's app drawer on a stock home screen — a
// whole-screen change, so it does not depend on any particular icon layout.
func TestDeviceSwipeChangesScreen(t *testing.T) {
	tr := deviceTransport(t)
	ctx := context.Background()

	before, err := tr.Screenshot(ctx)
	if err != nil {
		t.Fatalf("Screenshot() before: %v", err)
	}
	if err := tr.Swipe(ctx, Norm{X: 0.5, Y: 0.9}, Norm{X: 0.5, Y: 0.2}, 200*time.Millisecond); err != nil {
		t.Fatalf("Swipe(): %v", err)
	}
	time.Sleep(1500 * time.Millisecond) // let the drawer animation settle

	after, err := tr.Screenshot(ctx)
	if err != nil {
		t.Fatalf("Screenshot() after: %v", err)
	}
	t.Cleanup(func() { _ = tr.Back(context.Background()) })

	frac := changedFraction(before, after)
	t.Logf("changed fraction = %.4f", frac)
	if frac < minChangedFraction {
		t.Errorf("screen changed by %.4f, want at least %.2f: the gesture did not reach the device",
			frac, minChangedFraction)
	}
}

// minChangedFraction is how much of the screen must differ before we believe
// a gesture landed.
//
// The floor that matters is the status-bar clock: it occupies well under 1%
// of the screen, and a minute rolling over mid-test must not read as success.
// A false pass is the dangerous direction — it would let a transport that
// silently drops gestures look healthy. An opening app drawer changes most
// of the screen, so there is a wide margin to sit in.
const minChangedFraction = 0.05

// Per-channel tolerance, on the 0-65535 scale colour.Color reports. Software
// rendering (which headless emulation forces) dithers subtly between frames,
// so exact equality reports spurious differences on visually identical
// screens. ~1.5% of full scale absorbs that without hiding real changes.
const pixelTolerance = 1024

// sampleStride subsamples the grid. At 1080x2400 an exhaustive scan is 2.6M
// pixels per comparison; every 4th pixel in each axis is 16x cheaper and
// still 162k samples, which is ample for a "did the whole screen change"
// question. It would be the wrong tool for spotting a small badge appearing —
// that needs a region-restricted exact compare, not this.
const sampleStride = 4

// changedFraction reports the fraction of sampled pixels that differ between
// two screenshots, in [0,1]. Differing bounds count as a total change.
func changedFraction(a, b image.Image) float64 {
	ab, bb := a.Bounds(), b.Bounds()
	if ab != bb {
		return 1
	}

	var sampled, differing int
	for y := ab.Min.Y; y < ab.Max.Y; y += sampleStride {
		for x := ab.Min.X; x < ab.Max.X; x += sampleStride {
			ar, ag, ablue, _ := a.At(x, y).RGBA()
			br, bg, bblue, _ := b.At(x, y).RGBA()
			sampled++
			if absDiff(ar, br) > pixelTolerance ||
				absDiff(ag, bg) > pixelTolerance ||
				absDiff(ablue, bblue) > pixelTolerance {
				differing++
			}
		}
	}
	if sampled == 0 {
		return 0
	}
	return float64(differing) / float64(sampled)
}

func absDiff(x, y uint32) uint32 {
	if x > y {
		return x - y
	}
	return y - x
}
