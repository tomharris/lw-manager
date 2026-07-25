package studio_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/studio"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

func authed(t *testing.T, method, target string, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	return req
}

func TestUnsortedGridListsEveryUnlabelledFrame(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodGet, "/", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), f.Hash) {
		t.Fatal("the unsorted frame's hash is not on the page")
	}
	// Every known label must be offerable, including the negatives bucket.
	for _, want := range []string{"alliance_members", "vs_ranking", corpus.None} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("label %q is not offered on the page", want)
		}
	}
}

func TestPostLabelMovesTheFrame(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{"hash": {f.Hash}, "label": {"alliance"}}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/label", form.Encode()))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	got, err := store.Find(f.Hash)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Label != "alliance" {
		t.Fatalf("Label = %q, want alliance", got.Label)
	}
}

// The label arrives from a browser form, so a hostile value must not escape
// the corpus root.
func TestPostLabelRejectsAnUnsafeLabel(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{"hash": {f.Hash}, "label": {"../../etc"}}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/label", form.Encode()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	got, err := store.Find(f.Hash)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Label != corpus.Unsorted {
		t.Fatalf("Label = %q, want the frame left alone", got.Label)
	}
}

func TestPostCaptureStoresAFreshFrame(t *testing.T) {
	store := corpus.New(t.TempDir())
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	tr, err := transport.NewReplayTransportFromImages(img)
	if err != nil {
		t.Fatalf("NewReplayTransportFromImages: %v", err)
	}
	srv, err := studio.New(studio.Options{
		Corpus:       store,
		Transport:    tr,
		ManifestPath: t.TempDir() + "/manifest.yaml",
		RefHeight:    2400,
		Token:        "s3cret",
	})
	if err != nil {
		t.Fatalf("studio.New: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/capture", ""))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	frames, err := store.List(corpus.Unsorted)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("stored %d frames, want 1", len(frames))
	}
}

// Labelling a corpus on a machine with no phone must still work.
func TestPostCaptureWithoutATransportIsRejectedNotFatal(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret") // constructed with no Transport

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/capture", ""))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestLabeledBrowserGroupsFramesByLabel(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	if _, _, err := store.Add("alliance", []byte("a")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, _, err := store.Add(corpus.None, []byte("b")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodGet, "/labeled", ""))

	body := rec.Body.String()
	if !strings.Contains(body, "alliance") || !strings.Contains(body, corpus.None) {
		t.Fatalf("labeled page missing a label group:\n%s", body)
	}
}

func TestCropViewRendersTheFrameAndAnchorForm(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add("alliance", pngBytes(t, 1080, 2400))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodGet, "/crop?hash="+f.Hash, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/frame/" + f.Hash, `name="anchor_id"`, `name="identifies_screen"`} {
		if !strings.Contains(body, want) {
			t.Errorf("crop page missing %q", want)
		}
	}
}

func TestPostCropWritesTheTemplateAndManifest(t *testing.T) {
	store := corpus.New(t.TempDir())
	manifest := filepath.Join(t.TempDir(), "manifest.yaml")
	srv, err := studio.New(studio.Options{
		Corpus: store, ManifestPath: manifest, RefHeight: 2400, Token: "s3cret",
	})
	if err != nil {
		t.Fatalf("studio.New: %v", err)
	}
	f, _, err := store.Add("alliance", pngBytes(t, 1080, 2400))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{
		"hash": {f.Hash}, "screen": {"alliance"}, "anchor_id": {"alliance_button"},
		"x1": {"0.10"}, "y1": {"0.20"}, "x2": {"0.30"}, "y2": {"0.28"},
		"threshold": {"0.85"}, "identifies_screen": {"on"},
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/crop", form.Encode()))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	reg, err := vision.LoadRegistry(manifest)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if reg.ReferenceHeight != 2400 {
		t.Fatalf("ReferenceHeight = %d, want the frame's own height", reg.ReferenceHeight)
	}
	s, ok := reg.Screen("alliance")
	if !ok || len(s.Anchors) != 1 {
		t.Fatalf("registry screens = %+v, want one alliance anchor", reg.Screens)
	}
	a := s.Anchors[0]
	if a.ID != "alliance_button" || !a.IdentifiesScreen {
		t.Fatalf("anchor = %+v, want an identifying alliance_button", a)
	}
	// The template is the cropped region at the frame's native scale.
	if got := a.Template.Bounds().Dx(); got != int(0.20*1080) {
		t.Fatalf("template width = %d, want %d", got, int(0.20*1080))
	}
}

func TestPostCropRejectsAnInvertedRegion(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add("alliance", pngBytes(t, 1080, 2400))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{
		"hash": {f.Hash}, "screen": {"alliance"}, "anchor_id": {"bad"},
		"x1": {"0.9"}, "y1": {"0.9"}, "x2": {"0.1"}, "y2": {"0.1"},
		"threshold": {"0.85"},
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/crop", form.Encode()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// Mixing two capture resolutions in one library silently mis-scales every
// match, and reads as a merely-bad recognizer rather than an obvious break.
func TestPostCropRejectsAFrameAtTheWrongReferenceHeight(t *testing.T) {
	srv, store := newTestServer(t, "s3cret") // RefHeight 2400
	f, _, err := store.Add("alliance", pngBytes(t, 720, 1600))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{
		"hash": {f.Hash}, "screen": {"alliance"}, "anchor_id": {"a"},
		"x1": {"0.1"}, "y1": {"0.1"}, "x2": {"0.3"}, "y2": {"0.2"},
		"threshold": {"0.85"},
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/crop", form.Encode()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestPostCropRejectsAnUnsafeScreenName(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add("alliance", pngBytes(t, 1080, 2400))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{
		"hash": {f.Hash}, "screen": {"../../etc"}, "anchor_id": {"a"},
		"x1": {"0.1"}, "y1": {"0.1"}, "x2": {"0.3"}, "y2": {"0.2"},
		"threshold": {"0.85"},
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authed(t, http.MethodPost, "/crop", form.Encode()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// pngBytes builds a valid PNG of the given size with enough variation that a
// crop of it is a meaningful template.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding %dx%d PNG: %v", w, h, err)
	}
	return buf.Bytes()
}

// handleLabel's redirect target is derived from Referer, which is untrusted
// input: it can be absent (privacy-hardened browsers, extensions, non-browser
// clients), point off-host, or exploit a parser disagreement between this
// server and the browser (see safeRedirectTarget). All of that must fall
// back to "/" rather than producing an empty Location (which re-requests
// /label as a GET, and only POST is registered — a 404 mid-labelling) or
// sending the operator off-site.
//
// postLabelWithReferer posts a valid /label request carrying the given
// Referer (or no header at all, when setReferer is false) and returns the
// resulting Location header. It is shared by the safeRedirectTarget cases
// below, which all differ only in the Referer they send.
func postLabelWithReferer(t *testing.T, setReferer bool, referer string) string {
	t.Helper()
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	form := url.Values{"hash": {f.Hash}, "label": {"alliance"}}
	req := authed(t, http.MethodPost, "/label", form.Encode())
	if setReferer {
		req.Header.Set("Referer", referer)
	} else {
		req.Header.Del("Referer")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	return rec.Header().Get("Location")
}

func TestPostLabelWithNoRefererRedirectsHome(t *testing.T) {
	if loc := postLabelWithReferer(t, false, ""); loc != "/" {
		t.Fatalf("Location = %q, want exactly /", loc)
	}
}

func TestPostLabelWithAnOffHostRefererRedirectsHomeNotOffSite(t *testing.T) {
	if loc := postLabelWithReferer(t, true, "https://evil.example/whatever"); loc != "/" {
		t.Fatalf("Location = %q, want exactly / (not the off-host referer)", loc)
	}
}

// Go's url.Parse follows RFC 3986, under which a backslash is not a path
// separator, so this parses with an empty Host and looks like a harmless
// relative path. Browsers follow the WHATWG URL spec, which normalizes "\"
// to "/" before resolving Location, turning this exact string into
// "//evil.example/phish" — a protocol-relative redirect straight off-site.
// Host-comparison on the server's own parse of the string cannot catch this,
// because the server and the browser parse it into different things; only
// never echoing the header at all closes it.
func TestPostLabelWithABackslashRefererRedirectsHome(t *testing.T) {
	if loc := postLabelWithReferer(t, true, `/\evil.example/phish`); loc != "/" {
		t.Fatalf("Location = %q, want exactly / (not a backslash-normalized off-site redirect)", loc)
	}
}

// A scheme-relative reference ("//host/path") is a valid off-host redirect
// target in its own right, with no scheme or backslash trick needed.
func TestPostLabelWithASchemeRelativeRefererRedirectsHome(t *testing.T) {
	if loc := postLabelWithReferer(t, true, "//evil.example/phish"); loc != "/" {
		t.Fatalf("Location = %q, want exactly / (not the scheme-relative referer)", loc)
	}
}
