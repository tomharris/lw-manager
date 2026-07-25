package vision_test

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// writeAnchorCorruptChildEnv, when set in the environment, identifies this
// process as the re-exec'd subprocess for
// TestWriteAnchorRestoresACorruptedTemplateWhenTheReplaceWriteFails rather
// than a normal test run; writeAnchorCorruptChildDirEnv carries the temp dir
// the parent already seeded. The child never re-execs itself: it only ever
// reaches runWriteAnchorCorruptChild, which calls os.Exit instead of
// returning control to the testing framework.
const (
	writeAnchorCorruptChildEnv    = "LW_VISION_WRITE_ANCHOR_CORRUPT_CHILD"
	writeAnchorCorruptChildDirEnv = "LW_VISION_WRITE_ANCHOR_CORRUPT_DIR"

	// templateWriteMargin sits strictly between the original template's
	// size and the replacement's: room for the byte-for-byte restore write,
	// not for the oversized replacement. Nothing else writes in the child
	// process, so unlike a limit set on a shared, long-lived test binary,
	// this doesn't need headroom for anything but the restore itself.
	templateWriteMargin = 4096
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
//
// RLIMIT_FSIZE and the SIGXFSZ disposition are both process-wide, and a Go
// test binary is one process shared by every test in the package. Setting
// them here directly used to leave both in effect for whatever ran next:
// the limit once collided with Go's own testlog (the out-of-band file-access
// record the test cache uses, which lives for the whole binary run, not just
// this test, and can write at any moment), and the ignored signal was never
// reset. So the corrupting write happens in a throwaway subprocess instead:
// the parent below seeds the good template, then re-execs the test binary
// directly — not through `go test`, which is what keeps the testlog
// instrumentation out of it — with a marker environment variable. The
// child, identified by that marker in runWriteAnchorCorruptChild, sets the
// limit on itself, attempts the replacement, and exits; only the parent
// makes assertions.
func TestWriteAnchorRestoresACorruptedTemplateWhenTheReplaceWriteFails(t *testing.T) {
	if os.Getenv(writeAnchorCorruptChildEnv) != "" {
		runWriteAnchorCorruptChild(os.Getenv(writeAnchorCorruptChildDirEnv))
		return // unreachable: runWriteAnchorCorruptChild always calls os.Exit
	}

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

	// Re-exec this same test binary directly (never through `go test`) as
	// the one process that carries the hostile rlimit. A generous timeout
	// plus exec.CommandContext means a hung child gets killed rather than
	// leaked, and t.TempDir() cleans up dir regardless of how this test
	// ends.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWriteAnchorRestoresACorruptedTemplateWhenTheReplaceWriteFails$")
	cmd.Env = append(os.Environ(),
		writeAnchorCorruptChildEnv+"=1",
		writeAnchorCorruptChildDirEnv+"="+dir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("child process timed out before reporting the file-size-limited template write:\n%s", output)
		}
		t.Fatalf("child process did not report the expected file-size-limited template write failure: %v\n%s", err, output)
	}

	after, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("reading template after the child's write attempt: %v\nchild output:\n%s", err, output)
	}
	if !bytes.Equal(original, after) {
		t.Fatalf("template not restored after a truncated write: got %d bytes, want the original %d back\nchild output:\n%s", len(after), len(original), output)
	}
	if _, err := vision.LoadRegistry(manifest); err != nil {
		t.Fatalf("registry no longer loads after a rolled-back template write: %v\nchild output:\n%s", err, output)
	}
}

// runWriteAnchorCorruptChild is the subprocess side of
// TestWriteAnchorRestoresACorruptedTemplateWhenTheReplaceWriteFails. It runs
// in a re-exec'd copy of the test binary, invoked as a plain executable
// rather than through `go test`, so setting a process-wide RLIMIT_FSIZE and
// ignoring SIGXFSZ here never touches the shared test binary every other
// test in this package runs in. It never re-execs itself — the marker check
// in the caller only routes here once — and it asserts nothing beyond its
// own setup: its only job is to leave the template file on disk corrupted
// and then restored (or not, if the bug under test has regressed) for the
// parent to inspect once it exits. It always calls os.Exit and never
// returns control to the testing framework.
func runWriteAnchorCorruptChild(dir string) {
	tmplPath := filepath.Join(dir, "alliance", "alliance_button.png")
	original, err := os.ReadFile(tmplPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: reading seeded template: %v\n", err)
		os.Exit(2)
	}

	// A replacement strictly larger than the original, padded with filler
	// bytes past the valid PNG data — it is never expected to be decoded,
	// only to overrun the size limit below.
	replacementBytes := append(append([]byte{}, original...), bytes.Repeat([]byte{0}, 131072)...)

	// Exceeding RLIMIT_FSIZE delivers SIGXFSZ, whose default disposition
	// kills the process; ignoring it makes the write syscall return EFBIG
	// instead. Both are process-wide, but this process exists only to make
	// this one write and then exit, so nothing else is affected.
	signal.Ignore(syscall.SIGXFSZ)
	var limited syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		fmt.Fprintf(os.Stderr, "child: Getrlimit: %v\n", err)
		os.Exit(2)
	}
	limited.Cur = uint64(len(original)) + templateWriteMargin
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		fmt.Fprintf(os.Stderr, "child: Setrlimit: %v\n", err)
		os.Exit(2)
	}

	manifest := filepath.Join(dir, "manifest.yaml")
	replacement := spec("alliance", "alliance_button")
	replacement.Threshold = 0.5
	writeErr := vision.WriteAnchor(manifest, 2400, replacement, replacementBytes)
	if writeErr == nil {
		fmt.Fprintln(os.Stderr, "child: WriteAnchor did not report the file-size-limited template write; RLIMIT_FSIZE did not constrain it as expected")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "child: WriteAnchor failed as expected: %v\n", writeErr)
	os.Exit(0)
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
