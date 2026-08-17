package vision_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// buildActionsRegistry writes a manifest with one identifying anchor and one
// action anchor per named screen, so a single helper can build both the
// "one screen" and "two screen" registries the tests below need.
func buildActionsRegistry(t *testing.T, screens ...string) *vision.Registry {
	t.Helper()
	manifest := filepath.Join(t.TempDir(), "manifest.yaml")
	region := transport.Rect{X1: 0, Y1: 0, X2: 1, Y2: 1}
	for _, screen := range screens {
		if err := vision.WriteAnchor(manifest, 40, vision.AnchorSpec{
			Screen: screen, ID: screen + "_id_anchor", Region: region,
			Threshold: 0.9, IdentifiesScreen: true,
		}, tinyPNG(t)); err != nil {
			t.Fatalf("WriteAnchor identifying: %v", err)
		}
		if err := vision.WriteAnchor(manifest, 40, vision.AnchorSpec{
			Screen: screen, ID: screen + "_action", Region: region,
			Threshold: 0.9, IdentifiesScreen: false,
		}, tinyPNG(t)); err != nil {
			t.Fatalf("WriteAnchor action: %v", err)
		}
	}
	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	return reg
}

func TestActionAnchorsExcludesIdentifyingAnchors(t *testing.T) {
	reg := buildActionsRegistry(t, "alliance_members")

	anchors, err := vision.ActionAnchors(reg, "")
	if err != nil {
		t.Fatalf("ActionAnchors: %v", err)
	}
	if len(anchors) != 1 {
		t.Fatalf("ActionAnchors = %+v, want exactly the one action anchor", anchors)
	}
}

func TestActionAnchorsFiltersByScreen(t *testing.T) {
	reg := buildActionsRegistry(t, "alliance_members", "mail")

	anchors, err := vision.ActionAnchors(reg, "mail")
	if err != nil {
		t.Fatalf("ActionAnchors: %v", err)
	}
	if len(anchors) != 1 {
		t.Fatalf("ActionAnchors(mail) = %+v, want one anchor", anchors)
	}
	if anchors[0].Screen != "mail" {
		t.Fatalf("ActionAnchors(mail) returned screen %q, want mail", anchors[0].Screen)
	}
}

func TestActionAnchorsUnknownScreenErrors(t *testing.T) {
	reg := buildActionsRegistry(t, "alliance_members", "mail")

	_, err := vision.ActionAnchors(reg, "does_not_exist")
	if err == nil {
		t.Fatal("ActionAnchors(does_not_exist) = nil error, want one naming valid screens")
	}
	if !errors.Is(err, vision.ErrUnknownScreen) {
		t.Fatalf("ActionAnchors error = %v, want it to wrap ErrUnknownScreen", err)
	}
	for _, name := range []string{"alliance_members", "mail"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("ActionAnchors error %q does not list valid screen %q", err.Error(), name)
		}
	}
}

func TestEvaluateActionsProducesInAndOutObservations(t *testing.T) {
	reg := buildActionsRegistry(t, "alliance_members")

	frames := []vision.Frame{
		{Hash: "a", Label: "alliance_members", Image: decodePNG(t, tinyFrame(t, 40, 40))},
		{Hash: "b", Label: "alliance_members", Image: decodePNG(t, tinyFrame(t, 40, 40))},
		{Hash: "c", Label: "mail", Image: decodePNG(t, tinyFrame(t, 40, 40))},
	}

	obs, err := vision.EvaluateActions(reg, frames, "")
	if err != nil {
		t.Fatalf("EvaluateActions: %v", err)
	}
	if len(obs) != 3 {
		t.Fatalf("observations = %d, want 3 (one action anchor x three frames)", len(obs))
	}

	var inCount, outCount int
	for _, o := range obs {
		if o.AnchorID != "alliance_members_action" || o.Screen != "alliance_members" {
			t.Fatalf("observation = %+v, want the alliance_members_action anchor", o)
		}
		if o.FrameLabel == o.Screen {
			inCount++
		} else {
			outCount++
		}
	}
	if inCount != 2 || outCount != 1 {
		t.Fatalf("in/out = %d/%d, want 2/1", inCount, outCount)
	}
}

func TestEvaluateActionsHonoursScreenFilter(t *testing.T) {
	reg := buildActionsRegistry(t, "alliance_members", "mail")

	frames := []vision.Frame{
		{Hash: "a", Label: "alliance_members", Image: decodePNG(t, tinyFrame(t, 40, 40))},
		{Hash: "b", Label: "mail", Image: decodePNG(t, tinyFrame(t, 40, 40))},
	}

	obs, err := vision.EvaluateActions(reg, frames, "mail")
	if err != nil {
		t.Fatalf("EvaluateActions: %v", err)
	}
	// Only the mail action anchor should be scored, against both frames.
	if len(obs) != 2 {
		t.Fatalf("observations = %d, want 2 (one action anchor x two frames)", len(obs))
	}
	for _, o := range obs {
		if o.Screen != "mail" || o.AnchorID != "mail_action" {
			t.Fatalf("observation = %+v, want only the mail_action anchor", o)
		}
	}
}

func TestEvaluateActionsUnknownScreenErrors(t *testing.T) {
	reg := buildActionsRegistry(t, "alliance_members")

	_, err := vision.EvaluateActions(reg, nil, "does_not_exist")
	if !errors.Is(err, vision.ErrUnknownScreen) {
		t.Fatalf("EvaluateActions error = %v, want it to wrap ErrUnknownScreen", err)
	}
}

// The action scan and the recognition report must not overlap: an
// identifying anchor's observations belong to Evaluate/Separations as they
// already exist, not to this new path, or the same anchor would be measured
// (and potentially double-counted) by both.
func TestEvaluateActionsExcludesIdentifyingAnchorObservations(t *testing.T) {
	reg := buildActionsRegistry(t, "alliance_members")

	frames := []vision.Frame{
		{Hash: "a", Label: "alliance_members", Image: decodePNG(t, tinyFrame(t, 40, 40))},
	}

	obs, err := vision.EvaluateActions(reg, frames, "")
	if err != nil {
		t.Fatalf("EvaluateActions: %v", err)
	}
	for _, o := range obs {
		if o.AnchorID == "alliance_members_id_anchor" {
			t.Fatalf("EvaluateActions produced an observation for the identifying anchor: %+v", o)
		}
	}

	_, evalObs, err := vision.Evaluate(reg, frames)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, o := range evalObs {
		if o.AnchorID == "alliance_members_action" {
			t.Fatalf("Evaluate (recognition pass) produced an observation for the action anchor: %+v", o)
		}
	}
}
