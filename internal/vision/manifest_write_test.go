package vision_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// tinyPNG returns a valid 4x4 PNG so template loading succeeds.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8(x * 60)})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding test PNG: %v", err)
	}
	return buf.Bytes()
}

func spec(screen, id string) vision.AnchorSpec {
	return vision.AnchorSpec{
		Screen:           screen,
		ID:               id,
		Region:           transport.Rect{X1: 0.1, Y1: 0.1, X2: 0.4, Y2: 0.3},
		Threshold:        0.85,
		IdentifiesScreen: true,
	}
}

func TestWriteAnchorCreatesTheManifestAndTemplate(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")

	if err := vision.WriteAnchor(manifest, 2400, spec("alliance", "alliance_button"), tinyPNG(t)); err != nil {
		t.Fatalf("WriteAnchor: %v", err)
	}

	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry after write: %v", err)
	}
	if reg.ReferenceHeight != 2400 {
		t.Fatalf("ReferenceHeight = %d, want 2400", reg.ReferenceHeight)
	}
	s, ok := reg.Screen("alliance")
	if !ok {
		t.Fatal("screen alliance missing from the registry")
	}
	if len(s.Anchors) != 1 || s.Anchors[0].ID != "alliance_button" {
		t.Fatalf("anchors = %+v, want one alliance_button", s.Anchors)
	}
	if !s.Anchors[0].IdentifiesScreen {
		t.Fatal("IdentifiesScreen was not persisted")
	}
}

func TestWriteAnchorReplacesAnAnchorWithTheSameID(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	if err := vision.WriteAnchor(manifest, 2400, spec("alliance", "alliance_button"), tinyPNG(t)); err != nil {
		t.Fatalf("first WriteAnchor: %v", err)
	}

	updated := spec("alliance", "alliance_button")
	updated.Threshold = 0.7
	if err := vision.WriteAnchor(manifest, 2400, updated, tinyPNG(t)); err != nil {
		t.Fatalf("second WriteAnchor: %v", err)
	}

	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	s, _ := reg.Screen("alliance")
	if len(s.Anchors) != 1 {
		t.Fatalf("anchors = %+v, want the anchor replaced, not duplicated", s.Anchors)
	}
	if s.Anchors[0].Threshold != 0.7 {
		t.Fatalf("Threshold = %v, want 0.7", s.Anchors[0].Threshold)
	}
}

// A half-written manifest that fails validation is worse than no write: the
// failure would surface hours later, on a different command.
func TestWriteAnchorRollsBackWhenTheResultWouldNotLoad(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	good := spec("alliance", "alliance_button")
	if err := vision.WriteAnchor(manifest, 2400, good, tinyPNG(t)); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}

	bad := spec("alliance", "broken")
	bad.Region = transport.Rect{X1: 0.9, Y1: 0.9, X2: 0.1, Y2: 0.1} // inverted

	if err := vision.WriteAnchor(manifest, 2400, bad, tinyPNG(t)); err == nil {
		t.Fatal("WriteAnchor accepted an inverted region")
	}

	after, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading manifest after rollback: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("manifest changed despite a failed write:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(dir, "alliance", "broken.png")); !os.IsNotExist(err) {
		t.Fatal("the rejected template PNG was left behind")
	}
	if _, err := vision.LoadRegistry(manifest); err != nil {
		t.Fatalf("registry no longer loads after a rolled-back write: %v", err)
	}
}

func TestWriteAnchorRejectsAChangeOfReferenceHeight(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	if err := vision.WriteAnchor(manifest, 2400, spec("alliance", "a"), tinyPNG(t)); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := vision.WriteAnchor(manifest, 1920, spec("mail", "b"), tinyPNG(t)); err == nil {
		t.Fatal("WriteAnchor silently mixed templates captured at two reference heights")
	}
}

func TestSetThresholdsUpdatesNamedAnchorsOnly(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	for _, s := range []vision.AnchorSpec{spec("alliance", "a"), spec("mail", "b")} {
		if err := vision.WriteAnchor(manifest, 2400, s, tinyPNG(t)); err != nil {
			t.Fatalf("seeding %s/%s: %v", s.Screen, s.ID, err)
		}
	}

	if err := vision.SetThresholds(manifest, map[string]float64{"alliance/a": 0.62}); err != nil {
		t.Fatalf("SetThresholds: %v", err)
	}

	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	alliance, _ := reg.Screen("alliance")
	if alliance.Anchors[0].Threshold != 0.62 {
		t.Fatalf("alliance/a threshold = %v, want 0.62", alliance.Anchors[0].Threshold)
	}
	mail, _ := reg.Screen("mail")
	if mail.Anchors[0].Threshold != 0.85 {
		t.Fatalf("mail/b threshold = %v, want 0.85 unchanged", mail.Anchors[0].Threshold)
	}
}

func TestSetThresholdsRejectsAnUnknownAnchor(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	if err := vision.WriteAnchor(manifest, 2400, spec("alliance", "a"), tinyPNG(t)); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := vision.SetThresholds(manifest, map[string]float64{"alliance/nope": 0.5}); err == nil {
		t.Fatal("SetThresholds silently ignored an anchor that does not exist")
	}
}
