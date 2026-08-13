package runtime

import (
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"math/rand"
	"time"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// Recognition is a recognized screen with the recognizer's confidence.
type Recognition struct {
	Screen     string
	Confidence float64
}

// Options configures a Ctx. Transport, Registry, Graph, and Kill are
// required; the rest default sensibly.
type Options struct {
	Transport transport.Transport
	Registry  *vision.Registry
	Graph     *Graph
	Kill      KillSwitch
	// Capture persists frames for analytics tasks. Nil is allowed: Ctx
	// works without one, and Capture() then errors.
	Capture   Capturer
	AccountID int64

	// Rand drives all jitter. Nil means time-seeded; tests inject a seeded
	// source for determinism.
	Rand *rand.Rand
	// PollInterval paces recognition attempts inside WaitFor. Default 500ms.
	PollInterval time.Duration
	// WaitTimeout bounds WaitFor. Default 20s.
	WaitTimeout time.Duration
	// RestartTimeout bounds the wait for the entry screen after an app
	// restart — game boots are slow. Default 90s.
	RestartTimeout time.Duration
	Log            *slog.Logger
}

// Ctx is the only surface task code gets. Every primitive checks the kill
// switch before acting, requires recognition before any interaction, and
// jitters all timing. There is deliberately no accessor for the underlying
// Transport.
type Ctx struct {
	tr             transport.Transport
	reg            *vision.Registry
	rec            *vision.Recognizer
	graph          *Graph
	ks             KillSwitch
	cap            Capturer // nil ⇒ Capture unavailable
	accountID      int64
	rand           *rand.Rand
	poll           time.Duration
	waitTimeout    time.Duration
	restartTimeout time.Duration
	log            *slog.Logger
	screenshotIDs  []int64
}

// New builds a Ctx, validating the graph against the registry so a bad edge
// fails at startup rather than mid-task.
func New(opts Options) (*Ctx, error) {
	if opts.Transport == nil || opts.Registry == nil || opts.Graph == nil || opts.Kill == nil {
		return nil, errors.New("runtime: Transport, Registry, Graph and Kill are all required")
	}
	if err := opts.Graph.Validate(opts.Registry); err != nil {
		return nil, err
	}
	r := opts.Rand
	if r == nil {
		r = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	c := &Ctx{
		tr:             opts.Transport,
		reg:            opts.Registry,
		rec:            vision.NewRecognizer(opts.Registry),
		graph:          opts.Graph,
		ks:             opts.Kill,
		cap:            opts.Capture,
		accountID:      opts.AccountID,
		rand:           r,
		poll:           opts.PollInterval,
		waitTimeout:    opts.WaitTimeout,
		restartTimeout: opts.RestartTimeout,
		log:            opts.Log,
	}
	if c.poll <= 0 {
		c.poll = 500 * time.Millisecond
	}
	if c.waitTimeout <= 0 {
		c.waitTimeout = 20 * time.Second
	}
	if c.restartTimeout <= 0 {
		c.restartTimeout = 90 * time.Second
	}
	if c.log == nil {
		c.log = slog.Default().With("component", "runtime", "account", opts.AccountID)
	}
	return c, nil
}

// recognize captures one frame and recognizes it. Errors from the vision
// layer pass through raw; callers translate ErrNoScreenRecognized into
// recovery (recognizeOrRecover) so it never crosses the package boundary.
func (c *Ctx) recognize(ctx context.Context) (Recognition, image.Image, error) {
	frame, err := c.tr.Screenshot(ctx)
	if err != nil {
		return Recognition{}, nil, fmt.Errorf("runtime: screenshotting for account %d: %w", c.accountID, err)
	}
	screen, conf, err := c.rec.Recognize(frame)
	if err != nil {
		return Recognition{}, frame, err
	}
	return Recognition{Screen: screen, Confidence: conf}, frame, nil
}

// recognizeOrRecover recognizes the current frame, invoking the panic route
// when nothing matches. Task code above this line never sees
// vision.ErrNoScreenRecognized.
func (c *Ctx) recognizeOrRecover(ctx context.Context) (Recognition, error) {
	r, _, err := c.recognize(ctx)
	if errors.Is(err, vision.ErrNoScreenRecognized) {
		return c.panicRoute(ctx)
	}
	return r, err
}

// CurrentScreen reports which screen the device shows right now.
func (c *Ctx) CurrentScreen(ctx context.Context) (Recognition, error) {
	if err := c.ks.Check(ctx); err != nil {
		return Recognition{}, err
	}
	return c.recognizeOrRecover(ctx)
}

// wrongScreenStreak is how many consecutive identical wrong-screen sightings
// make WaitFor give up early: a stable recognized screen will not become a
// different one by staring at it, and callers (NavigateTo) can re-plan from
// the screen WaitFor reports.
const wrongScreenStreak = 5

// WaitFor polls until the named screen is recognized. It fails with
// ErrWrongScreen when a different screen shows steadily, and with the panic
// route's verdict when nothing is recognized for the whole timeout.
func (c *Ctx) WaitFor(ctx context.Context, screen string) (Recognition, error) {
	deadline := time.Now().Add(c.waitTimeout)
	streakScreen, streak := "", 0
	for {
		if err := c.ks.Check(ctx); err != nil {
			return Recognition{}, err
		}
		r, _, err := c.recognize(ctx)
		switch {
		case err == nil && r.Screen == screen:
			return r, nil
		case err == nil:
			if r.Screen == streakScreen {
				streak++
			} else {
				streakScreen, streak = r.Screen, 1
			}
			if streak >= wrongScreenStreak {
				return Recognition{}, fmt.Errorf("runtime: waiting for %q but device sits on %q: %w", screen, r.Screen, ErrWrongScreen)
			}
		case errors.Is(err, vision.ErrNoScreenRecognized):
			streakScreen, streak = "", 0 // unrecognized frames reset the streak
		default:
			return Recognition{}, err
		}

		if time.Now().After(deadline) {
			// Nothing recognizable for the whole budget: recover, then let
			// the caller decide what to do from wherever we ended up.
			r, rerr := c.panicRoute(ctx)
			if rerr != nil {
				return Recognition{}, rerr
			}
			if r.Screen == screen {
				return r, nil
			}
			return Recognition{}, fmt.Errorf("runtime: waited %s for %q, recovered onto %q: %w", c.waitTimeout, screen, r.Screen, ErrWrongScreen)
		}
		if err := c.Sleep(ctx, c.poll, 2*c.poll); err != nil {
			return Recognition{}, err
		}
	}
}

// Sleep waits a jittered duration in [min, max]. The only sanctioned wait.
func (c *Ctx) Sleep(ctx context.Context, min, max time.Duration) error {
	if err := c.ks.Check(ctx); err != nil {
		return err
	}
	t := time.NewTimer(Jitter(c.rand, min, max))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Resolution reports the device's screen size in pixels. It is a fixed
// device property rather than an action, so unlike every other primitive on
// Ctx it does not check the kill switch — reading it cannot touch the
// device. Task code needs it to reason about a region in absolute pixels,
// such as the scroll capture helper's usable-height calculation.
func (c *Ctx) Resolution() image.Point { return c.tr.Resolution() }

// Screenshot captures the current frame without recognizing or verifying it
// against any screen. It exists for callers that need to compare raw pixels
// across two points in time — such as measuring how far a list scrolled —
// where a full recognition pass would cost time without adding information.
// Most task code wants CurrentScreen or Capture instead; this is for the
// narrow case of a bare pixel comparison.
func (c *Ctx) Screenshot(ctx context.Context) (image.Image, error) {
	if err := c.ks.Check(ctx); err != nil {
		return nil, err
	}
	frame, err := c.tr.Screenshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("runtime: screenshotting for account %d: %w", c.accountID, err)
	}
	return frame, nil
}

// CheckKillSwitch exposes the kill-switch check directly, for task code that
// needs to bail out of a multi-step loop between steps that otherwise never
// touch Ctx (invariant #8: checked between every task step).
func (c *Ctx) CheckKillSwitch(ctx context.Context) error {
	return c.ks.Check(ctx)
}
