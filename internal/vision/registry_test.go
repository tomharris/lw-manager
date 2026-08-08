package vision

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
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

// The alliance tech recommendation badge is the motivating case: the same
// icon appears on a tab header and on a tech button. If one template served
// both anchors, region would be the only thing telling them apart, and
// overlapping regions would let each match the other's instance — tech_donate
// would then tap what it believed was a tech, switch tabs instead, and record
// a donation it never made, with no error and no log to show for it.
func TestLoadRegistryRejectsSharedTemplateWithOverlappingRegions(t *testing.T) {
	const overlapping = `
reference_height: 64
screens:
  alliance_tech:
    anchors:
      - id: tab_recommended_badge
        template: badge.png
        region: [0, 0, 0.5, 0.5]
        threshold: 0.85
        identifies_screen: false
      - id: tech_recommended_badge
        template: badge.png
        region: [0.4, 0.4, 1, 1]
        threshold: 0.85
        identifies_screen: false
`
	path := writeManifest(t, overlapping, map[string]bool{"badge.png": true})
	_, err := LoadRegistry(path)
	if err == nil {
		t.Fatal("want an error: region is the only thing telling these two apart")
	}
	if !strings.Contains(err.Error(), "tech_recommended_badge") {
		t.Errorf("error should name the colliding anchors, got: %v", err)
	}
}

func TestLoadRegistryAcceptsSharedTemplateWithDisjointRegions(t *testing.T) {
	const disjoint = `
reference_height: 64
screens:
  alliance_tech:
    anchors:
      - id: tab_recommended_badge
        template: badge.png
        region: [0, 0, 1, 0.2]
        threshold: 0.85
        identifies_screen: false
      - id: tech_recommended_badge
        template: badge.png
        region: [0, 0.25, 1, 1]
        threshold: 0.85
        identifies_screen: false
`
	path := writeManifest(t, disjoint, map[string]bool{"badge.png": true})
	if _, err := LoadRegistry(path); err != nil {
		t.Fatalf("disjoint regions must load: %v", err)
	}
}

// Different templates may overlap freely — the constraint is about anchors
// that only region distinguishes, not about regions in general. base's help
// icon and collect bubble are content-addressed and deliberately search most
// of the screen; both must keep loading.
func TestLoadRegistryAllowsOverlapBetweenDifferentTemplates(t *testing.T) {
	const broad = `
reference_height: 64
screens:
  base:
    anchors:
      - id: help_all_button
        template: help.png
        region: [0, 0, 1, 1]
        threshold: 0.85
        identifies_screen: false
      - id: collect_bubble
        template: bubble.png
        region: [0, 0, 1, 1]
        threshold: 0.85
        identifies_screen: false
`
	path := writeManifest(t, broad, map[string]bool{"help.png": true, "bubble.png": true})
	if _, err := LoadRegistry(path); err != nil {
		t.Fatalf("different templates may overlap: %v", err)
	}
}

// The shipping manifest is the one that matters: a guard that only ever sees
// synthetic YAML would not notice Task 5's two badge regions colliding.
func TestShippingManifestLoads(t *testing.T) {
	if _, err := LoadRegistry("../../templates/manifest.yaml"); err != nil {
		t.Fatalf("the shipping manifest must load: %v", err)
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
