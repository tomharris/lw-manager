package studio_test

import (
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/studio"
	"github.com/tomharris/lw-manager/internal/transport"
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
