package vision_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
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

// TestWriteAnchorRestoresACorruptedTemplateWhenTheReplaceWriteFails guards
// the failure mode where os.WriteFile truncates a file before writing its
// new contents: if the write that replaces an existing template fails
// partway through (disk full, in production), the previously-good template
// is already gone by the time the error surfaces, while the manifest still
// names it as valid — the exact failure this package's rollback exists to
// prevent, just relocated from the manifest to the file it names.
//
// A chmod on the file or its directory does not reproduce this: an
// existing, already-open-for-write-permitted file can still be truncated
// regardless of the directory's mode, and denying write on the file itself
// makes the open() call fail outright, before any truncation — so the
// original content is never actually touched, and the test would pass
// whether or not the rollback code runs at all (verified by temporarily
// removing the rollback call: a permission-denied version of this test
// still passed). A per-process file-size limit (RLIMIT_FSIZE) does
// reproduce it: the O_TRUNC open succeeds, some bytes land, and the write
// is cut off mid-stream, corrupting the file for real.
//
// The limit has to sit strictly between the original template's size and
// the replacement's: too low, and the internal restore — which writes back
// exactly the original byte count — would be truncated by the same limit
// it is trying to recover from, making the scenario unrecoverable by any
// implementation and the test meaningless. Sized correctly, the replacing
// write overruns the limit and corrupts the file, while the restore's
// smaller write fits under it and succeeds — the same relationship a real
// disk-full failure has, since O_TRUNC frees the original content's space
// before the write, leaving room for something no larger than what was
// just freed.
func TestWriteAnchorRestoresACorruptedTemplateWhenTheReplaceWriteFails(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("RLIMIT_FSIZE is POSIX-only")
	}

	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.yaml")
	original := tinyPNG(t)
	good := spec("alliance", "alliance_button")
	if err := vision.WriteAnchor(manifest, 2400, good, original); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	tmplPath := filepath.Join(dir, "alliance", "alliance_button.png")
	onDisk, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("reading seeded template: %v", err)
	}
	if !bytes.Equal(onDisk, original) {
		t.Fatalf("seeded template on disk (%d bytes) does not match what was written (%d bytes)", len(onDisk), len(original))
	}

	// A replacement strictly larger than the original, padded with filler
	// bytes past the valid PNG data — it is never expected to be decoded,
	// only to overrun the size limit below.
	replacementBytes := append(append([]byte{}, original...), bytes.Repeat([]byte{0}, 131072)...)

	// Exceeding RLIMIT_FSIZE delivers SIGXFSZ, whose default disposition
	// kills the process; ignoring it makes the write syscall return EFBIG
	// instead.
	signal.Ignore(syscall.SIGXFSZ)
	var oldLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &oldLimit); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	limited := oldLimit
	// Room for the restore write, not for the replacement. This margin has
	// to be generous, not just "a few bytes": RLIMIT_FSIZE is process-wide,
	// so while it is lowered it also constrains Go's own out-of-band test
	// cache instrumentation (the "testlog" that records file accesses for
	// cache invalidation and lives for the whole test binary run, not just
	// this test), which can append to an unrelated file at any moment
	// during the run. Measured at ~11KB for this package's suite as of the
	// Evaluate/Rescale tests added in task 13; a margin of a few bytes or
	// even 4KB was observed to make that unrelated write fail with "file
	// too large" well before this test's own assertions run. 64KB is
	// generous headroom over that, without being so large a real disk-full
	// write of an oversized replacement could no longer exceed it.
	limited.Cur = uint64(len(original)) + 65536
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatalf("Setrlimit: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &oldLimit) })

	replacement := spec("alliance", "alliance_button")
	replacement.Threshold = 0.5
	writeErr := vision.WriteAnchor(manifest, 2400, replacement, replacementBytes)

	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &oldLimit); err != nil {
		t.Fatalf("restoring RLIMIT_FSIZE: %v", err)
	}
	if writeErr == nil {
		t.Fatal("WriteAnchor did not report the file-size-limited template write")
	}

	after, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("reading template after the failed write: %v", err)
	}
	if !bytes.Equal(original, after) {
		t.Fatalf("template not restored after a truncated write: got %d bytes, want the original %d back", len(after), len(original))
	}
	if _, err := vision.LoadRegistry(manifest); err != nil {
		t.Fatalf("registry no longer loads after a rolled-back template write: %v", err)
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
