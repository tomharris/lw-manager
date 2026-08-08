package vision

import (
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sort"

	yaml "go.yaml.in/yaml/v3"

	"github.com/tomharris/lw-manager/internal/transport"
)

// Anchor is one template to look for: its image, where on the screen to look
// (a normalized region), and how good the NCC match must be to count. An
// anchor that IdentifiesScreen participates in deciding which screen we are
// on; others exist only as tap targets once the screen is known.
type Anchor struct {
	ID               string
	Template         image.Image
	Region           transport.Rect
	Threshold        float64
	IdentifiesScreen bool
}

// Screen is a named collection of anchors — a recognizable game screen.
type Screen struct {
	Name    string
	Anchors []Anchor
}

// Registry is the loaded template library: the reference resolution its
// templates were captured at, and every screen it can recognize. It is the
// versioned source of truth a game update would force a re-capture of, which
// is why templates live in one directory behind one manifest.
type Registry struct {
	ReferenceHeight int
	Screens         []Screen // sorted by name for deterministic recognition
}

// Screen returns the named screen, or ok=false.
func (r *Registry) Screen(name string) (Screen, bool) {
	for _, s := range r.Screens {
		if s.Name == name {
			return s, true
		}
	}
	return Screen{}, false
}

// on-disk manifest shapes, kept private so the public types stay clean.
type manifestFile struct {
	ReferenceHeight int                       `yaml:"reference_height"`
	Screens         map[string]screenManifest `yaml:"screens"`
}

type screenManifest struct {
	Anchors []anchorManifest `yaml:"anchors"`
}

type anchorManifest struct {
	ID               string     `yaml:"id"`
	Template         string     `yaml:"template"`
	Region           [4]float64 `yaml:"region"`
	Threshold        float64    `yaml:"threshold"`
	IdentifiesScreen bool       `yaml:"identifies_screen"`
}

// LoadRegistry reads a YAML manifest and every template PNG it names, relative
// to the manifest's own directory. It validates loudly — an inverted region,
// an out-of-range threshold, or a missing template file is a manifest bug that
// must fail at load, not surface as a mysterious never-matching anchor later.
func LoadRegistry(manifestPath string) (*Registry, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("vision: reading manifest %s: %w", manifestPath, err)
	}
	var mf manifestFile
	if err := yaml.Unmarshal(raw, &mf); err != nil {
		return nil, fmt.Errorf("vision: parsing manifest %s: %w", manifestPath, err)
	}
	if mf.ReferenceHeight <= 0 {
		return nil, fmt.Errorf("vision: manifest %s: reference_height must be positive, got %d", manifestPath, mf.ReferenceHeight)
	}

	dir := filepath.Dir(manifestPath)
	reg := &Registry{ReferenceHeight: mf.ReferenceHeight}

	for name, sm := range mf.Screens {
		s := Screen{Name: name}
		for _, am := range sm.Anchors {
			region := transport.Rect{X1: am.Region[0], Y1: am.Region[1], X2: am.Region[2], Y2: am.Region[3]}
			if !region.Valid() {
				return nil, fmt.Errorf("vision: manifest %s: screen %q anchor %q has invalid region %v", manifestPath, name, am.ID, am.Region)
			}
			if am.Threshold < 0 || am.Threshold > 1 {
				return nil, fmt.Errorf("vision: manifest %s: screen %q anchor %q threshold %.3f outside [0,1]", manifestPath, name, am.ID, am.Threshold)
			}
			img, err := loadPNG(filepath.Join(dir, am.Template))
			if err != nil {
				return nil, fmt.Errorf("vision: manifest %s: screen %q anchor %q: %w", manifestPath, name, am.ID, err)
			}
			s.Anchors = append(s.Anchors, Anchor{
				ID:               am.ID,
				Template:         img,
				Region:           region,
				Threshold:        am.Threshold,
				IdentifiesScreen: am.IdentifiesScreen,
			})
		}
		if err := checkSharedTemplateRegions(manifestPath, name, sm.Anchors); err != nil {
			return nil, err
		}
		reg.Screens = append(reg.Screens, s)
	}
	sort.Slice(reg.Screens, func(i, j int) bool { return reg.Screens[i].Name < reg.Screens[j].Name })
	return reg, nil
}

// checkSharedTemplateRegions rejects two anchors on one screen that name the
// same template file and whose search regions overlap.
//
// The alliance tech recommendation badge is the case: the same icon appears on
// a tab header and on a tech button, and if one PNG served both anchors then
// region would be the only thing telling them apart. Overlapping regions let
// each match the other's instance, and the resulting failure is silent — a
// task taps what it believes is one and gets the other, with no error to show
// for it. Today's manifest cuts two separate PNGs, so this guard is a trap set
// for the recrop that consolidates them, not a check on current data.
//
// Anchors with different templates may overlap freely; base's help icon and
// collect bubble both search most of the screen by design, because neither has
// a fixed position.
func checkSharedTemplateRegions(manifestPath, screen string, anchors []anchorManifest) error {
	for i := 0; i < len(anchors); i++ {
		for j := i + 1; j < len(anchors); j++ {
			if anchors[i].Template != anchors[j].Template {
				continue
			}
			a := transport.Rect{X1: anchors[i].Region[0], Y1: anchors[i].Region[1], X2: anchors[i].Region[2], Y2: anchors[i].Region[3]}
			b := transport.Rect{X1: anchors[j].Region[0], Y1: anchors[j].Region[1], X2: anchors[j].Region[2], Y2: anchors[j].Region[3]}
			if a.X1 < b.X2 && b.X1 < a.X2 && a.Y1 < b.Y2 && b.Y1 < a.Y2 {
				return fmt.Errorf("vision: manifest %s: screen %q anchors %q and %q share template %s with overlapping regions; region is the only thing telling them apart",
					manifestPath, screen, anchors[i].ID, anchors[j].ID, anchors[i].Template)
			}
		}
	}
	return nil
}

func loadPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening template %s: %w", path, err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decoding template %s: %w", path, err)
	}
	return img, nil
}
