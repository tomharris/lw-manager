//go:build scrolldiag

// Package-internal diagnostic for investigating ScrollOffset against real
// handset frames. Not part of any suite; run by hand:
//
//	go test -tags scrolldiag ./internal/vision -run TestScrollDiag -v \
//	    -args -frames /path/r0.png,/path/r1.png,... -pitch 112
//
// It reports, for each consecutive pair, every probe's full score-vs-offset
// curve reduced to: the winning offset, its score, and the best competing
// placement more than one strip away. The last number is the one that matters
// — a uniform list correlates with itself at every multiple of the row pitch,
// so the competition is never noise, it is "off by N rows", which is exactly
// the error that would mis-segment a capture.
package vision

import (
	"flag"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

var (
	diagFrames = flag.String("frames", "", "comma-separated frames in capture order")
	diagPitch  = flag.Int("pitch", 112, "row pitch, for annotating decoys")
	diagTrue   = flag.Int("true", -1, "independently measured true offset, if known")
	diagY1     = flag.Float64("y1", 0.44, "region Y1")
	diagY2     = flag.Float64("y2", 0.89, "region Y2")
	diagX1     = flag.Float64("x1", 0.03, "region X1")
	diagX2     = flag.Float64("x2", 0.97, "region X2")
)

func diagLoadPNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return img
}

// diagCurve is one probe's score at every candidate offset.
type diagCurve struct {
	y      int
	varnce float64
	score  []float64 // indexed by d
}

// peak returns the highest-scoring offset, and the best score at least
// `apart` pixels away from it — the "off by N rows" competitor.
func (c diagCurve) peak(apart int) (bestD int, best, decoy float64) {
	best = -2
	for d, s := range c.score {
		if s > best {
			best, bestD = s, d
		}
	}
	decoy = -2
	for d, s := range c.score {
		if d < bestD-apart || d > bestD+apart {
			if s > decoy {
				decoy = s
			}
		}
	}
	return bestD, best, decoy
}

func TestScrollDiag(t *testing.T) {
	if *diagFrames == "" {
		t.Skip("need -frames")
	}
	paths := strings.Split(*diagFrames, ",")
	if len(paths) < 2 {
		t.Fatal("need at least two frames")
	}

	region := transport.Rect{X1: *diagX1, Y1: *diagY1, X2: *diagX2, Y2: *diagY2}
	fmt.Printf("pitch=%d region=%+v\n", *diagPitch, region)
	fmt.Printf("%-10s %-6s %8s %8s %8s %8s   %s\n",
		"pair", "probe", "d", "peak", "decoy", "margin", "decoy offsets from peak")

	prev := diagLoadPNG(t, paths[0])
	for i := 1; i < len(paths); i++ {
		cur := diagLoadPNG(t, paths[i])
		h := cur.Bounds().Dy()
		regionTop := int(region.Y1 * float64(h))
		regionBot := int(region.Y2 * float64(h))
		stripH := int(offsetStripFrac * float64(regionBot-regionTop))

		var curves []diagCurve
		for p := 0; p < offsetProbes; p++ {
			// Probes start AT the region top, not one strip below it. Each
			// step down costs stripH of measurable travel, because a probe can
			// only find content that is still above the region's bottom edge
			// in the previous frame.
			y := regionTop + p*stripH
			r := transport.Rect{X1: region.X1, Y1: float64(y) / float64(h), X2: region.X2, Y2: float64(y+stripH) / float64(h)}
			sub := Crop(cur, r)
			c := diagCurve{y: y, varnce: variance(sub)}
			for oy := y; oy+stripH <= regionBot; oy++ {
				sr := transport.Rect{X1: region.X1, Y1: float64(oy) / float64(h), X2: region.X2, Y2: float64(oy+stripH) / float64(h)}
				res, err := Match(prev, sub, sr, h)
				if err != nil {
					t.Fatal(err)
				}
				c.score = append(c.score, res.Score)
			}
			curves = append(curves, c)
		}

		pair := fmt.Sprintf("%d->%d", i-1, i)
		// When the true offset is known independently (measured by eye off the
		// frames), print its score alongside the winner's. A winner that is not
		// the truth is the failure this whole exercise exists to detect.
		if *diagTrue >= 0 {
			for p, c := range curves {
				if *diagTrue < len(c.score) {
					fmt.Printf("%-10s probe %d  TRUE d=%d scores %.4f\n", pair, p, *diagTrue, c.score[*diagTrue])
				}
			}
		}
		for p, c := range curves {
			d, best, decoy := c.peak(stripH)
			// A probe's range limit is the whole story of the VS failure: a
			// probe whose limit is below the true offset cannot see it and
			// returns a confident lattice decoy instead of an error.
			limit := regionBot - c.y - stripH
			fmt.Printf("%-10s %-6d %8d %8.4f %8.4f %8.4f  limit=%d%s  %s\n",
				pair, p, d, best, decoy, best-decoy, limit,
				map[bool]string{true: " BLIND", false: ""}[*diagTrue >= 0 && *diagTrue > limit],
				diagDecoys(c, d, stripH, *diagPitch))
		}

		// The combined curve: probes averaged. Each probe is an independent
		// look at the same shift, but the lattice decoys are NOT shared —
		// probe 0's strongest competitor is rarely probe 1's — so averaging
		// should sharpen the true peak relative to them. Whether it actually
		// does is the question this line answers.
		comb := diagCurve{y: curves[0].y}
		for d := range curves[0].score {
			sum := 0.0
			for _, c := range curves {
				if d < len(c.score) {
					sum += c.score[d]
				}
			}
			comb.score = append(comb.score, sum/float64(len(curves)))
		}
		d, best, decoy := comb.peak(stripH)
		fmt.Printf("%-10s %-6s %8d %8.4f %8.4f %8.4f   %s\n",
			pair, "COMB", d, best, decoy, best-decoy, diagDecoys(comb, d, stripH, *diagPitch))

		prev = cur
	}
}

// diagDecoys names the strongest competitors as row counts away from the
// winning offset, which is how a lattice artefact announces itself.
func diagDecoys(c diagCurve, peakD, apart, pitch int) string {
	type hit struct {
		d int
		s float64
	}
	var hits []hit
	for d, s := range c.score {
		if d < peakD-apart || d > peakD+apart {
			hits = append(hits, hit{d, s})
		}
	}
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			if hits[j].s > hits[i].s {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}
	var parts []string
	for k := 0; k < 3 && k < len(hits); k++ {
		delta := hits[k].d - peakD
		parts = append(parts, fmt.Sprintf("%+d(%+.2f rows)@%.3f",
			delta, float64(delta)/float64(pitch), hits[k].s))
	}
	return strings.Join(parts, " ")
}
