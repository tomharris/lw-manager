package vision

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writeManifest lays out a manifest.yaml plus its template PNGs in a temp dir
// and returns the manifest path. Everything the registry loads is synthetic —
// no captured game screen is needed to exercise the loader.
func writeManifest(t *testing.T, yaml string, templates map[string]bool) string {
	t.Helper()
	dir := t.TempDir()
	for name := range templates {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, distinctTemplate(8, 8)); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodManifest = `
reference_height: 64
screens:
  world_map:
    anchors:
      - id: btn_alliance
        template: btn_alliance.png
        region: [0.5, 0.5, 1.0, 1.0]
        threshold: 0.87
        identifies_screen: true
`

func TestLoadRegistryParsesAnchors(t *testing.T) {
	path := writeManifest(t, goodManifest, map[string]bool{"btn_alliance.png": true})

	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if reg.ReferenceHeight != 64 {
		t.Errorf("reference_height: got %d, want 64", reg.ReferenceHeight)
	}
	s, ok := reg.Screen("world_map")
	if !ok {
		t.Fatalf("world_map screen not found")
	}
	if len(s.Anchors) != 1 {
		t.Fatalf("anchors: got %d, want 1", len(s.Anchors))
	}
	a := s.Anchors[0]
	if a.ID != "btn_alliance" || a.Threshold != 0.87 || !a.IdentifiesScreen {
		t.Errorf("anchor fields wrong: %+v", a)
	}
	if a.Region.X1 != 0.5 || a.Region.Y2 != 1.0 {
		t.Errorf("region not parsed: %+v", a.Region)
	}
	if a.Template == nil {
		t.Errorf("template image not loaded")
	}
}

func TestLoadRegistryRejectsMissingTemplate(t *testing.T) {
	path := writeManifest(t, goodManifest, map[string]bool{}) // no PNG written
	if _, err := LoadRegistry(path); err == nil {
		t.Fatal("expected error for missing template file, got nil")
	}
}

func TestLoadRegistryRejectsBadRegion(t *testing.T) {
	const bad = `
reference_height: 64
screens:
  world_map:
    anchors:
      - id: btn_alliance
        template: btn_alliance.png
        region: [0.5, 0.5, 0.4, 1.0]   # x2 < x1: inverted
        threshold: 0.87
        identifies_screen: true
`
	path := writeManifest(t, bad, map[string]bool{"btn_alliance.png": true})
	if _, err := LoadRegistry(path); err == nil {
		t.Fatal("expected error for inverted region, got nil")
	}
}

func TestLoadRegistryRejectsBadThreshold(t *testing.T) {
	const bad = `
reference_height: 64
screens:
  world_map:
    anchors:
      - id: btn_alliance
        template: btn_alliance.png
        region: [0.5, 0.5, 1.0, 1.0]
        threshold: 1.5   # out of [0,1]
`
	path := writeManifest(t, bad, map[string]bool{"btn_alliance.png": true})
	if _, err := LoadRegistry(path); err == nil {
		t.Fatal("expected error for threshold > 1, got nil")
	}
}
