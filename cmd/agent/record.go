package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image/png"
	"log/slog"
	"math/rand"
	"time"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
)

// pngSource adapts a Transport to corpus.FrameSource: the transport yields an
// image.Image, and the corpus stores encoded bytes.
type pngSource struct{ tr transport.Transport }

func (s pngSource) Frame(ctx context.Context) ([]byte, error) {
	img, err := s.tr.Screenshot(ctx)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encoding frame: %w", err)
	}
	return buf.Bytes(), nil
}

func runRecord(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	serial := fs.String("serial", "", "device serial; optional when exactly one device is attached")
	root := fs.String("corpus", "fixtures/corpus", "corpus root directory")
	interval := fs.Duration("interval", 2*time.Second, "nominal wait between frames; jittered ±40%")
	count := fs.Int("count", 0, "stop after N frames; 0 means run until --duration or Ctrl-C")
	duration := fs.Duration("duration", 0, "stop after this long; 0 means run until --count or Ctrl-C")
	pkg := fs.String("package", transport.DefaultPackage, "game package name, for the version stamp")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolved, err := resolveSerial(ctx, cfg.ADBPath, *serial)
	if err != nil {
		return err
	}
	tr, err := transport.NewADBTransport(ctx, transport.ADBOptions{
		ADBPath: cfg.ADBPath,
		Serial:  resolved,
		Package: *pkg,
	})
	if err != nil {
		return fmt.Errorf("opening device %s: %w", resolved, err)
	}
	defer tr.Close()

	model, gameVersion, err := transport.DeviceProps(ctx, cfg.ADBPath, resolved, *pkg)
	if err != nil {
		return err
	}
	res := tr.Resolution()

	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	// Jitter the wait rather than sleeping a fixed interval. Recording is
	// human-driven, but fixed timing is the most detectable signal we emit
	// and there is no reason for this loop to be the exception (invariant #7).
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	low := time.Duration(float64(*interval) * 0.6)
	high := time.Duration(float64(*interval) * 1.4)
	sleep := func(ctx context.Context) error {
		t := time.NewTimer(runtime.Jitter(rng, low, high))
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}

	store := corpus.New(*root)
	result, err := corpus.Record(ctx, store, pngSource{tr: tr}, corpus.RecordOptions{
		Count: *count,
		Sleep: sleep,
		Meta: corpus.Meta{
			Width:       res.X,
			Height:      res.Y,
			CapturedAt:  time.Now().UTC(),
			Device:      model,
			GameVersion: gameVersion,
		},
	})
	if err != nil {
		return err
	}

	// Fold this session's metadata into the index straight away, so a
	// forgotten `corpus index` cannot lose the device and version stamps.
	prev, err := store.ReadIndex()
	if err != nil {
		return err
	}
	frames, err := store.All()
	if err != nil {
		return err
	}
	if err := store.WriteIndex(corpus.Reindex(prev, frames, result.Metas)); err != nil {
		return err
	}

	// stdout stays machine-readable; it must appear whether or not the
	// duplicate check below finds anything. An operator who just captured
	// 200 frames needs to see what survived even when this run also
	// surfaces a pre-existing defect.
	fmt.Printf("captured=%d duplicates=%d corpus=%s device=%s resolution=%dx%d game_version=%s\n",
		result.Captured, result.Duplicates, *root, model, res.X, res.Y, gameVersion)

	// Report, don't refuse, on a cross-label duplicate here — the opposite of
	// what `agent corpus index` does, deliberately. corpus.Record writes
	// through Store.Add, which dedups by content hash against the whole
	// tree before a second copy of the same bytes is ever created, so
	// record itself cannot produce this condition; a duplicate found here
	// predates the session (a `cp` where a `mv` was meant). This session's
	// Meta — device model, game version, capture timestamps — exists only
	// in memory until the WriteIndex above, and the frames are already
	// durably on disk, so refusing to write would destroy real, freshly
	// captured metadata to punish a defect this run did not cause. Warn
	// loudly, exit non-zero so the defect is not missed, but only after the
	// index and summary are safely out. `agent corpus index` hard-fails
	// instead because producing a trustworthy canonical index is its entire
	// job, and there is nothing of this session's to lose there.
	if dupes := corpus.Duplicates(frames); len(dupes) > 0 {
		slog.Warn("corpus has frames filed under more than one label; index was still written",
			"hashes", dupes, "fix", "delete the copy under the wrong label, then run: agent corpus index")
		return fmt.Errorf("corpus: %d frame(s) filed under more than one label (see warning above)", len(dupes))
	}
	return nil
}
