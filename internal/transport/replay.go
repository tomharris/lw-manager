package transport

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrFramesExhausted reports that a replay exceeded its screenshot budget.
var ErrFramesExhausted = errors.New("transport: replay frames exhausted")

// DefaultMaxServes caps how many screenshots one replay will serve. Generous
// enough for any legitimate poll loop, small enough that a runaway task fails
// in under a second rather than hanging the suite.
const DefaultMaxServes = 200

// Action is one recorded interaction, so tests can assert what a task did
// rather than only what it returned.
type Action struct {
	Kind     string // "tap", "swipe", "type", "restart"
	At       Norm
	To       Norm
	Duration time.Duration
	Text     string
}

// ReplayTransport serves recorded screenshots from a fixture directory and
// records every interaction instead of performing one.
//
// This exists so the platform's invariant #6 is achievable: all vision logic
// ships with fixture-based tests that run with no device attached. It is
// written before ADBTransport is trusted, and the entire vision milestone
// will be developed against it.
type ReplayTransport struct {
	// MaxServes overrides DefaultMaxServes for a test that legitimately needs
	// a longer or shorter screenshot budget. Zero means use the default.
	MaxServes int

	mu      sync.Mutex
	frames  []image.Image
	names   []string
	idx     int
	size    image.Point
	actions []Action
}

// NewReplayTransport loads every .png/.jpg in dir, sorted by filename.
// Sorting by name makes replay order deterministic and lets fixture authors
// control sequence with a numeric prefix (010_world_map.png, 020_alliance.png).
func NewReplayTransport(dir string) (*ReplayTransport, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("transport: reading fixture dir %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".png", ".jpg", ".jpeg":
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	if len(names) == 0 {
		return nil, fmt.Errorf("transport: no image fixtures in %s", dir)
	}

	t := &ReplayTransport{names: names}
	for _, n := range names {
		img, err := decodeFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		t.frames = append(t.frames, img)
	}

	b := t.frames[0].Bounds()
	t.size = image.Point{X: b.Dx(), Y: b.Dy()}
	return t, nil
}

// NewReplayTransportFromImages builds a replay from in-memory images, for
// tests that would rather generate frames than keep fixture files.
func NewReplayTransportFromImages(imgs ...image.Image) (*ReplayTransport, error) {
	if len(imgs) == 0 {
		return nil, errors.New("transport: at least one frame is required")
	}
	b := imgs[0].Bounds()
	names := make([]string, len(imgs))
	for i := range imgs {
		names[i] = fmt.Sprintf("frame-%03d", i)
	}
	return &ReplayTransport{
		frames: imgs,
		names:  names,
		size:   image.Point{X: b.Dx(), Y: b.Dy()},
	}, nil
}

func decodeFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("transport: opening fixture %s: %w", path, err)
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("transport: decoding fixture %s: %w", path, err)
	}
	return img, nil
}

// nextFrame returns the frame to serve for the current Screenshot call and
// advances internal state. Callers hold t.mu.
//
// Exhaustion policy is hold-last-frame with a cap. Once the fixtures run out
// the final frame repeats, so a poll-until-recognized loop settles on a
// stable screen the way it would against a real idle device. But serves are
// capped, so a task that never converges fails with ErrFramesExhausted
// instead of spinning until the test suite times out.
//
// The two halves matter for different reasons: holding the last frame keeps
// panic-route tests honest (the route legitimately captures more frames than
// it stages, since it spams back up to 5x before restarting), while the cap
// keeps a genuine infinite loop diagnosable.
func (t *ReplayTransport) nextFrame() (image.Image, error) {
	if t.idx >= t.maxServes() {
		return nil, fmt.Errorf("transport: served %d frames from %d fixtures without converging: %w",
			t.idx, len(t.frames), ErrFramesExhausted)
	}

	i := t.idx
	if i >= len(t.frames) {
		i = len(t.frames) - 1 // hold the last frame
	}
	t.idx++
	return t.frames[i], nil
}

// maxServes is the total Screenshot budget before the replay gives up.
func (t *ReplayTransport) maxServes() int {
	if t.MaxServes > 0 {
		return t.MaxServes
	}
	return DefaultMaxServes
}

func (t *ReplayTransport) Screenshot(ctx context.Context) (image.Image, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nextFrame()
}

func (t *ReplayTransport) Tap(ctx context.Context, p Norm) error {
	if err := checkPoints(p); err != nil {
		return err
	}
	t.record(Action{Kind: "tap", At: p})
	return nil
}

func (t *ReplayTransport) Swipe(ctx context.Context, from, to Norm, d time.Duration) error {
	if err := checkPoints(from, to); err != nil {
		return err
	}
	t.record(Action{Kind: "swipe", At: from, To: to, Duration: d})
	return nil
}

func (t *ReplayTransport) TypeText(ctx context.Context, s string) error {
	t.record(Action{Kind: "type", Text: s})
	return nil
}

func (t *ReplayTransport) AppRestart(ctx context.Context) error {
	t.record(Action{Kind: "restart"})
	return nil
}

func (t *ReplayTransport) Resolution() image.Point { return t.size }

func (t *ReplayTransport) Close() error { return nil }

func (t *ReplayTransport) record(a Action) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.actions = append(t.actions, a)
}

// Actions returns a copy of everything the transport was asked to do.
func (t *ReplayTransport) Actions() []Action {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Action, len(t.actions))
	copy(out, t.actions)
	return out
}

// Reset rewinds to the first frame and clears recorded actions.
func (t *ReplayTransport) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.idx = 0
	t.actions = nil
}

// EncodePNG is a helper for tests that need screenshot bytes rather than an
// image.Image.
func EncodePNG(img image.Image) ([]byte, error) {
	var buf strings.Builder
	if err := png.Encode(&stringWriter{&buf}, img); err != nil {
		return nil, fmt.Errorf("transport: encoding png: %w", err)
	}
	return []byte(buf.String()), nil
}

type stringWriter struct{ b *strings.Builder }

func (w *stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
