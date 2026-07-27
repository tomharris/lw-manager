package vision

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"

	yaml "go.yaml.in/yaml/v3"

	"github.com/tomharris/lw-manager/internal/corpus"
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
	// Screen and ID both become path segments below
	// (filepath.Join(dir, spec.Screen, spec.ID+".png")), so they are
	// validated with the rule the corpus package already uses for its own
	// label directories. The studio's handleCrop validates both one layer up
	// before it ever calls this function, but that is exactly the shape of
	// gap this branch shipped once already: a value checked at one layer and
	// assumed at another. The check belongs where the path is built, so the
	// next caller — which will not necessarily repeat handleCrop's check —
	// is covered too.
	if err := corpus.CheckLabel(spec.Screen); err != nil {
		return fmt.Errorf("vision: anchor screen: %w", err)
	}
	if err := corpus.CheckLabel(spec.ID); err != nil {
		return fmt.Errorf("vision: anchor id: %w", err)
	}
	if !spec.Region.Valid() {
		return fmt.Errorf("vision: anchor %s/%s region %+v is not a valid unit-square rect",
			spec.Screen, spec.ID, spec.Region)
	}
	if spec.Threshold < 0 || spec.Threshold > 1 {
		return fmt.Errorf("vision: anchor %s/%s threshold %.3f outside [0,1]",
			spec.Screen, spec.ID, spec.Threshold)
	}

	tmplImg, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return fmt.Errorf("vision: anchor %s/%s template is not a decodable PNG: %w", spec.Screen, spec.ID, err)
	}
	// LoadRegistry only decodes the PNG, so a flat crop of blank UI — a
	// perfectly valid, zero-variance PNG — would otherwise sail through both
	// this function and the revalidation below, and only fail later inside
	// Match with "template has zero variance (uniform), NCC undefined": not
	// ErrNoScreenRecognized, so the panic route never engages and the raw
	// error aborts every task on every screen.
	if variance(tmplImg) == 0 {
		return fmt.Errorf("vision: anchor %s/%s template has zero variance (a flat crop with no visible detail); crop a region with visible detail",
			spec.Screen, spec.ID)
	}
	// A region that cannot even place the exact template it was cropped
	// with will not survive Match against any real frame either — proven
	// here, before either is written, rather than left to surface later on
	// whatever screen happens to be recognized next.
	if err := verifyPlaceable(tmplImg, spec.Region, refHeight); err != nil {
		return fmt.Errorf("vision: anchor %s/%s region cannot place its own template: %w",
			spec.Screen, spec.ID, err)
	}

	mf, prevManifest, manifestExisted, err := readManifest(manifestPath)
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
	// os.WriteFile truncates before writing, so a failure partway through
	// (disk full, permissions revoked) can leave a previously-good template
	// corrupt while the manifest still names it as valid. Define the
	// restore before attempting the write so that failure is covered too,
	// not just the later manifest-write and revalidation failures.
	restoreTemplate := func() { restore(tmplPath, prevTemplate, hadTemplate) }

	if err := os.MkdirAll(filepath.Dir(tmplPath), 0o755); err != nil {
		return fmt.Errorf("vision: creating %s: %w", filepath.Dir(tmplPath), err)
	}
	if err := os.WriteFile(tmplPath, pngBytes, 0o644); err != nil {
		restoreTemplate()
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
		restore(manifestPath, prevManifest, manifestExisted)
		restoreTemplate()
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

// maxSyntheticFrameWidth bounds the synthetic frame verifyPlaceable builds.
// A region with an implausibly small width fraction (crafted or malformed —
// rectFromForm only checks ordering and unit-square bounds, not a minimum
// size) would otherwise force an arbitrarily large allocation before this
// function ever gets to say no.
const maxSyntheticFrameWidth = 20000

// verifyPlaceable proves an anchor's region can actually locate its own
// template, before either is written anywhere. It reconstructs a plausible
// parent frame: sized so the template, pasted at the region's declared
// offset, occupies exactly the fraction of the frame the region claims, at
// this manifest's own reference height. Running the real matcher against
// that reconstruction is what catches a region defined too small (or
// mismatched entirely) for the template it is paired with — the same shape
// of bug as the studio's own zero-slack crop, just arriving from whatever
// caller comes after the studio, which this package cannot assume will
// repeat handleCrop's padding.
func verifyPlaceable(tmpl image.Image, region transport.Rect, refHeight int) error {
	tb := tmpl.Bounds()
	rw := region.X2 - region.X1

	frameW := int(math.Round(float64(tb.Dx()) / rw))
	if frameW < tb.Dx() {
		frameW = tb.Dx()
	}
	if frameW > maxSyntheticFrameWidth {
		return fmt.Errorf("region width fraction %.6f is implausibly small for a %dpx-wide template", rw, tb.Dx())
	}
	frameH := refHeight

	g := Grayscale(tmpl)
	frame := image.NewGray(image.Rect(0, 0, frameW, frameH))
	px := int(math.Round(region.X1 * float64(frameW)))
	py := int(math.Round(region.Y1 * float64(frameH)))
	gb := g.Bounds()
	for y := 0; y < gb.Dy(); y++ {
		for x := 0; x < gb.Dx(); x++ {
			fx, fy := px+x, py+y
			if fx < 0 || fy < 0 || fx >= frameW || fy >= frameH {
				continue
			}
			frame.SetGray(fx, fy, g.GrayAt(gb.Min.X+x, gb.Min.Y+y))
		}
	}

	_, err := Match(frame, tmpl, region, refHeight)
	return err
}

// SetThresholds updates thresholds for anchors keyed "<screen>/<anchorID>".
//
// An unknown key is an error rather than a no-op: the only caller is
// `agent score --apply-thresholds`, and silently skipping a key there would
// report success while leaving the anchor that actually needs fixing alone.
func SetThresholds(manifestPath string, thresholds map[string]float64) error {
	mf, prevManifest, manifestExisted, err := readManifest(manifestPath)
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
		restore(manifestPath, prevManifest, manifestExisted)
		return err
	}
	if _, err := LoadRegistry(manifestPath); err != nil {
		restore(manifestPath, prevManifest, manifestExisted)
		return fmt.Errorf("vision: threshold update would break the manifest, rolled back: %w", err)
	}
	return nil
}

// readManifest loads the manifest for editing, returning its raw bytes and
// whether the file existed at all so a failed write can be rolled back
// exactly. Inferring "existed" from a non-empty byte slice would misclassify
// a pre-existing zero-byte manifest as absent and delete it instead of
// restoring it, so the existed bool is threaded explicitly, the same way
// readIfExists does for the template. A missing file is an empty manifest:
// the first crop creates it.
func readManifest(path string) (mf manifestFile, raw []byte, existed bool, err error) {
	raw, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return manifestFile{}, nil, false, nil
	}
	if err != nil {
		return manifestFile{}, nil, false, fmt.Errorf("vision: reading manifest %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &mf); err != nil {
		return manifestFile{}, nil, false, fmt.Errorf("vision: parsing manifest %s: %w", path, err)
	}
	return mf, raw, true, nil
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
