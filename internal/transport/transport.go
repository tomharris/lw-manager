// Package transport drives a device: pixels in, touches out.
//
// Invariant #1 of the platform is that no absolute pixel coordinate exists
// outside a Transport implementation. That is enforced here by the type
// system: the interface speaks only in Norm, and denormalisation to device
// pixels happens exclusively inside an implementation. A new device with a
// different resolution therefore needs no changes anywhere upstream.
package transport

import (
	"context"
	"fmt"
	"image"
	"time"
)

// Norm is a resolution-independent point. Both components are in [0,1],
// measured from the top-left corner.
type Norm struct {
	X, Y float64
}

// Valid reports whether p lies inside the unit square.
func (p Norm) Valid() bool {
	return p.X >= 0 && p.X <= 1 && p.Y >= 0 && p.Y <= 1
}

// Pixels converts p to device pixels for a screen of the given size.
// This is the only sanctioned way to produce absolute coordinates, and it is
// unexported-by-convention: call it from transport implementations only.
func (p Norm) Pixels(size image.Point) (int, int) {
	return int(p.X * float64(size.X)), int(p.Y * float64(size.Y))
}

// Rect is a resolution-independent rectangle, used by the vision layer to
// describe search regions and match bounding boxes.
type Rect struct {
	X1, Y1, X2, Y2 float64
}

// Center returns the midpoint of r.
func (r Rect) Center() Norm {
	return Norm{X: (r.X1 + r.X2) / 2, Y: (r.Y1 + r.Y2) / 2}
}

// Valid reports whether r lies inside the unit square and is non-inverted.
func (r Rect) Valid() bool {
	return r.X1 >= 0 && r.Y1 >= 0 && r.X2 <= 1 && r.Y2 <= 1 && r.X1 < r.X2 && r.Y1 < r.Y2
}

// Transport is one device: an emulator instance or a physical handset.
//
// Implementations must be safe for use by one caller at a time. The scheduler
// guarantees that a device never runs two tasks concurrently, so transports
// need not be internally synchronised.
type Transport interface {
	// Screenshot captures the current screen.
	Screenshot(ctx context.Context) (image.Image, error)
	// Tap touches a single point.
	Tap(ctx context.Context, p Norm) error
	// Swipe drags from one point to another over the given duration.
	Swipe(ctx context.Context, from, to Norm, d time.Duration) error
	// TypeText enters text into the focused field.
	TypeText(ctx context.Context, s string) error
	// AppRestart force-restarts the game. The panic route's last resort.
	AppRestart(ctx context.Context) error
	// Back presses the Android back button. Unlike Tap it needs no anchor:
	// it is the panic route's first resort, used precisely when the current
	// screen is unknown.
	Back(ctx context.Context) error
	// Resolution reports the device screen size in pixels.
	Resolution() image.Point
	// Close releases any resources held by the transport.
	Close() error
}

// ErrOutOfRange reports a coordinate outside the unit square. Returned rather
// than clamped: a coordinate outside [0,1] means an upstream bug, and
// silently clamping it would put a tap somewhere plausible but wrong.
type ErrOutOfRange struct {
	Point Norm
}

func (e ErrOutOfRange) Error() string {
	return fmt.Sprintf("transport: normalized point (%.4f, %.4f) is outside [0,1]", e.Point.X, e.Point.Y)
}

// checkPoints validates every point before it reaches a device.
func checkPoints(pts ...Norm) error {
	for _, p := range pts {
		if !p.Valid() {
			return ErrOutOfRange{Point: p}
		}
	}
	return nil
}
