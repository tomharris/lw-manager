package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/vision"
)

func runScore(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	root := fs.String("corpus", "fixtures/corpus", "corpus root directory")
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	gate := fs.Float64("gate", 0.98, "minimum accuracy; a lower score exits non-zero")
	rescale := fs.String("rescale", "", "comma-separated scale factors to also score, e.g. 0.75,1.25")
	asJSON := fs.Bool("json", false, "emit machine-readable output")
	apply := fs.Bool("apply-thresholds", false, "write suggested thresholds back to the manifest")
	actions := fs.Bool("actions", false, "score action anchors (tap targets) instead of running recognition")
	screenFilter := fs.String("screen", "", "with --actions, restrict the scan to one screen's action anchors")
	if err := fs.Parse(args); err != nil {
		return err
	}

	reg, err := vision.LoadRegistry(*manifest)
	if err != nil {
		return err
	}
	frames, err := vision.LoadCorpusFrames(corpus.New(*root))
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("corpus %s is empty; run `agent corpus pull` first", *root)
	}

	if *actions {
		return runScoreActions(reg, frames, *screenFilter, *asJSON)
	}
	if *screenFilter != "" {
		return fmt.Errorf("--screen only applies together with --actions")
	}

	preds, obs, err := vision.Evaluate(reg, frames)
	if err != nil {
		return err
	}
	report := vision.Score(preds)
	seps := vision.Separations(obs)

	type scaled struct {
		Factor   float64 `json:"factor"`
		Accuracy float64 `json:"accuracy"`
	}
	var rescaled []scaled
	for _, f := range parseFactors(*rescale) {
		scaledFrames := make([]vision.Frame, len(frames))
		for i, fr := range frames {
			scaledFrames[i] = vision.Frame{Hash: fr.Hash, Label: fr.Label, Image: vision.Rescale(fr.Image, f)}
		}
		p, _, err := vision.Evaluate(reg, scaledFrames)
		if err != nil {
			return fmt.Errorf("scoring at scale %.2f: %w", f, err)
		}
		rescaled = append(rescaled, scaled{Factor: f, Accuracy: vision.Score(p).Accuracy()})
	}

	if *apply {
		thresholds := map[string]float64{}
		for _, s := range seps {
			if s.Overlap {
				continue // no threshold can fix this one; recrop it
			}
			thresholds[s.Screen+"/"+s.AnchorID] = s.Suggested
		}
		if err := vision.SetThresholds(*manifest, thresholds); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "applied %d suggested thresholds to %s\n", len(thresholds), *manifest)
		// The accuracy, matrix, and separations printed below were computed
		// against the manifest as it was *before* this update — report,
		// preds, and seps are all already in hand by this point. The
		// manifest on disk no longer matches them, so nothing below is a
		// measurement of what was just written.
		fmt.Fprintln(os.Stderr, "warning: the figures below describe the manifest BEFORE these threshold updates;",
			"re-run `agent score` (without --apply-thresholds) to measure the manifest as now written",
			"before trusting the gate — gating on a corpus you just tuned against proves nothing.")
	}

	if *asJSON {
		out := map[string]any{
			"total": report.Total, "correct": report.Correct,
			"accuracy": report.Accuracy(), "gate": *gate,
			"passed": report.Accuracy() >= *gate,
			"matrix": report.Matrix, "separations": seps,
			"rescaled": rescaled,
			// Per-frame, so a misrecognition can be traced back to the frame
			// that caused it. The matrix says five base frames went to <none>;
			// only this says which five, and the hash is what the studio
			// opens. Emitted whole rather than errors-only: which rows are
			// interesting depends on the question being asked.
			"predictions": preds,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		fmt.Printf("accuracy %.4f (%d/%d) gate %.4f\n\n",
			report.Accuracy(), report.Correct, report.Total, *gate)
		fmt.Println(report.FormatMatrix())
		fmt.Println(vision.FormatSeparations(seps))
		for _, s := range rescaled {
			fmt.Printf("rescaled x%.2f: accuracy %.4f\n", s.Factor, s.Accuracy)
		}
		if len(rescaled) > 0 {
			// Say what this does and does not show, in the report itself.
			// A limitation stated only in a design doc is a limitation
			// nobody rereads.
			fmt.Println("\nrescaled figures test the matcher's scale handling only. A real second\n" +
				"device differs in DPI, so its layout and font hinting differ too; this is\n" +
				"not evidence of cross-device generalization.")
		}
	}

	if report.Accuracy() < *gate {
		return fmt.Errorf("accuracy %.4f is below the gate of %.4f", report.Accuracy(), *gate)
	}
	return nil
}

// runScoreActions is the --actions branch of `agent score`: it measures
// action anchors — the tap targets a task aims at, never the anchors that
// decide which screen a frame shows — instead of running recognition.
//
// Action anchors are invisible to the ordinary scoring pass by design
// (recognizer.go skips any anchor whose IdentifiesScreen is false, so
// Evaluate never produces an AnchorObservation for one). That leaves
// invariant #3 — no task acts without a matched anchor — resting entirely on
// anchors nothing measures. This is the separate, opt-in path that measures
// them, reusing Separations exactly as the recognition report does so the
// same worst-in/best-out/gap reading applies to both.
//
// There is no accuracy figure or gate here: Score/Report answer "did
// recognition call the right screen", which has no meaning for a tap target.
// The separation table is the whole answer for this path.
func runScoreActions(reg *vision.Registry, frames []vision.Frame, screen string, asJSON bool) error {
	obs, err := vision.EvaluateActions(reg, frames, screen)
	if err != nil {
		return err
	}
	seps := vision.Separations(obs)

	if asJSON {
		out := map[string]any{
			"screen":      screen,
			"separations": seps,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	fmt.Println(vision.FormatSeparations(seps))
	return nil
}

func parseFactors(s string) []float64 {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if f, err := strconv.ParseFloat(part, 64); err == nil && f > 0 {
			out = append(out, f)
		}
	}
	return out
}
