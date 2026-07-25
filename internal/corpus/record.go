package corpus

import (
	"context"
	"errors"
	"fmt"
)

// FrameSource yields one encoded frame at a time. It exists so the record
// loop can be tested with canned bytes: transport.Transport yields an
// image.Image, and turning that into PNG bytes is the caller's job.
type FrameSource interface {
	Frame(ctx context.Context) ([]byte, error)
}

// RecordOptions configures one recording session.
type RecordOptions struct {
	// Count caps the number of source reads. Zero means keep going until the
	// context ends.
	Count int
	// Sleep waits between frames. It is injected rather than taken from
	// internal/runtime so this package stays a filesystem leaf; the caller
	// supplies a jittered sleep and satisfies invariant #7 there.
	Sleep func(ctx context.Context) error
	// Meta is stamped on every frame captured during this run. It cannot be
	// recovered from a PNG's bytes, so it has to be carried in.
	Meta Meta
}

// RecordResult reports what a session produced.
type RecordResult struct {
	Captured   int
	Duplicates int
	Metas      map[string]Meta
}

// Record captures frames from src into the corpus under Unsorted until the
// context ends or Count frames have been read.
//
// Duplicates are counted but not stored: a burst capture of an idle screen
// yields the same bytes repeatedly, and keeping them would skew the accuracy
// denominator. A failure on the very first frame is returned, because that is
// almost always "no device attached" and an empty corpus with a zero exit
// status is the worst possible outcome. Later failures end the session with
// what was already captured intact.
func Record(ctx context.Context, s *Store, src FrameSource, opts RecordOptions) (RecordResult, error) {
	res := RecordResult{Metas: map[string]Meta{}}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = func(context.Context) error { return nil }
	}

	for i := 0; opts.Count == 0 || i < opts.Count; i++ {
		if err := ctx.Err(); err != nil {
			return res, nil
		}

		data, err := src.Frame(ctx)
		if err != nil {
			if i == 0 {
				return res, fmt.Errorf("corpus: capturing the first frame: %w", err)
			}
			return res, nil
		}

		f, added, err := s.Add(Unsorted, data)
		if err != nil {
			return res, err
		}
		if !added {
			res.Duplicates++
		} else {
			res.Captured++
			res.Metas[f.Hash] = opts.Meta
		}

		if err := sleep(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return res, nil
			}
			return res, err
		}
	}
	return res, nil
}
