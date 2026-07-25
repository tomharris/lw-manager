package studio_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/studio"
)

// seedFrame stores one frame and returns its hash, so auth tests can target a
// route that actually exists rather than a placeholder.
func seedFrame(t *testing.T, store *corpus.Store, body string) string {
	t.Helper()
	f, _, err := store.Add(corpus.Unsorted, []byte(body))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return f.Hash
}

func newTestServer(t *testing.T, token string) (*studio.Server, *corpus.Store) {
	t.Helper()
	store := corpus.New(t.TempDir())
	srv, err := studio.New(studio.Options{
		Corpus:       store,
		ManifestPath: t.TempDir() + "/manifest.yaml",
		RefHeight:    2400,
		Token:        token,
	})
	if err != nil {
		t.Fatalf("studio.New: %v", err)
	}
	return srv, store
}

func TestRequireTokenIsTrueOnlyForNonLoopbackBinds(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8088": false,
		"localhost:8088": false,
		"[::1]:8088":     false,
		"0.0.0.0:8088":   true,
		":8088":          true,
		"192.168.1.5:80": true,
	} {
		if got := studio.RequireToken(addr); got != want {
			t.Errorf("RequireToken(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	hash := seedFrame(t, store, "frame-bytes")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/frame/"+hash, nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// Auth is checked before routing, so an unknown path must still 401 rather
// than leaking which routes exist.
func TestUnauthenticatedRequestsTo404PathsAreStillRejected(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestTokenInQueryStringSetsACookieAndAdmits(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	hash := seedFrame(t, store, "frame-bytes")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/frame/"+hash+"?t=s3cret", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == studio.CookieName && c.Value == "s3cret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s cookie set; cookies = %v", studio.CookieName, rec.Result().Cookies())
	}
}

func TestCookieAdmitsAndAWrongTokenDoesNot(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	hash := seedFrame(t, store, "frame-bytes")

	ok := httptest.NewRequest(http.MethodGet, "/frame/"+hash, nil)
	ok.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	recOK := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOK, ok)
	if recOK.Code != http.StatusOK {
		t.Fatalf("valid cookie: status = %d, want 200", recOK.Code)
	}

	bad := httptest.NewRequest(http.MethodGet, "/frame/"+hash, nil)
	bad.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "wrong"})
	recBad := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBad, bad)
	if recBad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: status = %d, want 401", recBad.Code)
	}
}

func TestNewRejectsAnEmptyToken(t *testing.T) {
	if _, err := studio.New(studio.Options{
		Corpus:       corpus.New(t.TempDir()),
		ManifestPath: "manifest.yaml",
		RefHeight:    2400,
	}); !errors.Is(err, studio.ErrTokenRequired) {
		t.Fatalf("New with no token: err = %v, want ErrTokenRequired", err)
	}
}

func TestFrameEndpointServesTheStoredBytes(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/frame/"+f.Hash, nil)
	req.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "frame-bytes" {
		t.Fatalf("body = %q, want the stored bytes", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
}

func TestFrameEndpointIs404ForAnUnknownHash(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/frame/deadbeef", nil)
	req.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
