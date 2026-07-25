package vision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	yaml "go.yaml.in/yaml/v3"

	"github.com/tomharris/lw-manager/internal/transport"
)

// AnchorSpec describes an anchor to add to or replace in the manifest.
type AnchorSpec struct {
	Screen           string
	ID               string
	Region           transport.Rect
	Threshold        float64
	IdentifiesScreen bool
}

// WriteAnchor adds or replaces an anchor, writing its template PNG alongside
// the manifest.
//
// It re-runs LoadRegistry afterwards and rolls both writes back if the result
// no longer loads. LoadRegistry already validates loudly; reusing it as a
// write-time check is what keeps the manifest from ever reaching a state that
// breaks `agent run-task`. Without the rollback that would be merely likely
// rather than true: a half-written manifest surfaces hours later, on a
// different command, with no obvious cause.
func WriteAnchor(manifestPath string, refHeight int, spec AnchorSpec, pngBytes []byte) error {
	if spec.Screen == "" || spec.ID == "" {
		return fmt.Errorf("vision: anchor needs both a screen and an id, got %q/%q", spec.Screen, spec.ID)
	}
	if !spec.Region.Valid() {
		return fmt.Errorf("vision: anchor %s/%s region %+v is not a valid unit-square rect",
			spec.Screen, spec.ID, spec.Region)
	}
	if spec.Threshold < 0 || spec.Threshold > 1 {
		return fmt.Errorf("vision: anchor %s/%s threshold %.3f outside [0,1]",
			spec.Screen, spec.ID, spec.Threshold)
	}

	mf, prevManifest, err := readManifest(manifestPath)
	if err != nil {
		return err
	}
	if mf.ReferenceHeight == 0 {
		mf.ReferenceHeight = refHeight
	} else if mf.ReferenceHeight != refHeight {
		// One template library, one capture resolution. Mixing two silently
		// mis-scales every match, and the symptom is a recognizer that is
		// merely bad rather than obviously broken.
		return fmt.Errorf("vision: manifest %s was captured at reference height %d, refusing to add a template at %d",
			manifestPath, mf.ReferenceHeight, refHeight)
	}

	dir := filepath.Dir(manifestPath)
	rel := filepath.Join(spec.Screen, spec.ID+".png")
	tmplPath := filepath.Join(dir, rel)

	prevTemplate, hadTemplate, err := readIfExists(tmplPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(tmplPath), 0o755); err != nil {
		return fmt.Errorf("vision: creating %s: %w", filepath.Dir(tmplPath), err)
	}
	if err := os.WriteFile(tmplPath, pngBytes, 0o644); err != nil {
		return fmt.Errorf("vision: writing template %s: %w", tmplPath, err)
	}

	if mf.Screens == nil {
		mf.Screens = map[string]screenManifest{}
	}
	sm := mf.Screens[spec.Screen]
	entry := anchorManifest{
		ID:               spec.ID,
		Template:         filepath.ToSlash(rel),
		Region:           [4]float64{spec.Region.X1, spec.Region.Y1, spec.Region.X2, spec.Region.Y2},
		Threshold:        spec.Threshold,
		IdentifiesScreen: spec.IdentifiesScreen,
	}
	replaced := false
	for i := range sm.Anchors {
		if sm.Anchors[i].ID == spec.ID {
			sm.Anchors[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		sm.Anchors = append(sm.Anchors, entry)
	}
	sort.Slice(sm.Anchors, func(i, j int) bool { return sm.Anchors[i].ID < sm.Anchors[j].ID })
	mf.Screens[spec.Screen] = sm

	rollback := func() {
		restore(manifestPath, prevManifest, len(prevManifest) > 0)
		restore(tmplPath, prevTemplate, hadTemplate)
	}
	if err := writeManifestFile(manifestPath, mf); err != nil {
		rollback()
		return err
	}
	if _, err := LoadRegistry(manifestPath); err != nil {
		rollback()
		return fmt.Errorf("vision: anchor %s/%s would break the manifest, rolled back: %w",
			spec.Screen, spec.ID, err)
	}
	return nil
}

// SetThresholds updates thresholds for anchors keyed "<screen>/<anchorID>".
//
// An unknown key is an error rather than a no-op: the only caller is
// `agent score --apply-thresholds`, and silently skipping a key there would
// report success while leaving the anchor that actually needs fixing alone.
func SetThresholds(manifestPath string, thresholds map[string]float64) error {
	mf, prevManifest, err := readManifest(manifestPath)
	if err != nil {
		return err
	}

	applied := map[string]bool{}
	for name, sm := range mf.Screens {
		for i := range sm.Anchors {
			key := name + "/" + sm.Anchors[i].ID
			v, ok := thresholds[key]
			if !ok {
				continue
			}
			if v < 0 || v > 1 {
				return fmt.Errorf("vision: threshold %.3f for %s outside [0,1]", v, key)
			}
			sm.Anchors[i].Threshold = v
			applied[key] = true
		}
		mf.Screens[name] = sm
	}
	for key := range thresholds {
		if !applied[key] {
			return fmt.Errorf("vision: manifest %s has no anchor %q", manifestPath, key)
		}
	}

	if err := writeManifestFile(manifestPath, mf); err != nil {
		restore(manifestPath, prevManifest, len(prevManifest) > 0)
		return err
	}
	if _, err := LoadRegistry(manifestPath); err != nil {
		restore(manifestPath, prevManifest, len(prevManifest) > 0)
		return fmt.Errorf("vision: threshold update would break the manifest, rolled back: %w", err)
	}
	return nil
}

// readManifest loads the manifest for editing, returning its raw bytes so a
// failed write can be rolled back exactly. A missing file is an empty
// manifest: the first crop creates it.
func readManifest(path string) (manifestFile, []byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return manifestFile{}, nil, nil
	}
	if err != nil {
		return manifestFile{}, nil, fmt.Errorf("vision: reading manifest %s: %w", path, err)
	}
	var mf manifestFile
	if err := yaml.Unmarshal(raw, &mf); err != nil {
		return manifestFile{}, nil, fmt.Errorf("vision: parsing manifest %s: %w", path, err)
	}
	return mf, raw, nil
}

func writeManifestFile(path string, mf manifestFile) error {
	raw, err := yaml.Marshal(mf)
	if err != nil {
		return fmt.Errorf("vision: encoding manifest: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("vision: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("vision: writing manifest %s: %w", path, err)
	}
	return nil
}

func readIfExists(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("vision: reading %s: %w", path, err)
	}
	return raw, true, nil
}

// restore puts a file back the way it was, deleting it when it did not exist
// before. Errors here are unrecoverable and deliberately ignored: the caller
// is already returning the failure that triggered the rollback, and masking
// it with a rollback error would hide the real cause.
func restore(path string, data []byte, existed bool) {
	if existed {
		_ = os.WriteFile(path, data, 0o644)
		return
	}
	_ = os.Remove(path)
}
