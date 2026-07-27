package studio_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// A token in the query string sets the cookie and then redirects with the
// token stripped, rather than serving the request straight through — leaving
// it in place would keep the token in the address bar, browser history, and
// any outgoing Referer header on every request that happened to carry it.
func TestTokenInQueryStringSetsACookieAndRedirectsWithoutIt(t *testing.T) {
	srv, store := newTestServer(t, "s3cret")
	hash := seedFrame(t, store, "frame-bytes")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/frame/"+hash+"?t=s3cret", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "t=") {
		t.Fatalf("Location = %q, still carries the token", loc)
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

// A GET request-target beginning with "//" parses on the server, via Go's
// RFC 3986 request-target rules, as an ordinary path with an empty Host —
// authenticate's redirect used to echo it straight back in Location. A
// browser resolving that Location follows the WHATWG URL spec instead,
// which reads a leading "//" as a scheme-relative reference and navigates
// off-host, even though the server-side parse never saw a Host at all.
func TestTokenInQueryStringWithSchemeRelativePathRedirectsSameHost(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	req := httptest.NewRequest(http.MethodGet, "//evil.example/x?t=s3cret", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.HasPrefix(loc, "//") {
		t.Fatalf("Location = %q, a leading // is read by browsers as an off-host redirect", loc)
	}
}

// Go's RFC 3986 parser does not treat a backslash as a path separator, so
// this request-target parses with a literal backslash in Path. Browsers
// normalize "\" to "/" before resolving Location, turning the same string
// into "//evil.example/phish" once reflected back — the same off-host jump
// as the scheme-relative case above, reached a different way.
func TestTokenInQueryStringWithBackslashPathRedirectsSameHost(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")
	req := httptest.NewRequest(http.MethodGet, `/\evil.example/phish?t=s3cret`, nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.HasPrefix(loc, "//") || strings.HasPrefix(loc, `/\`) {
		t.Fatalf("Location = %q, browser-normalizes to an off-host redirect", loc)
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

// Go 1.22's ServeMux matches routes against the escaped path, so a
// percent-encoded "/" never splits a path segment and the whole remainder
// matches {hash} — but PathValue returns it decoded, real "/" and ".."
// included. Without a shape check at the corpus layer this reaches
// filepath.Join and reads outside the corpus root; this asserts the studio
// route as a whole does not let that through, whatever hash arrives.
func TestFrameEndpointRejectsPathTraversalInTheHash(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	req := httptest.NewRequest(http.MethodGet, "/frame/"+url.PathEscape("../../../etc/hosts"), nil)
	req.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, a traversal attempt must never be served")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestFrameEndpointIs404ForAnUnknownHash(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	// Well-formed (64 lowercase hex) but absent, so this tests "unknown"
	// specifically — the malformed case has its own test above.
	unknown := strings.Repeat("ab", 32)
	req := httptest.NewRequest(http.MethodGet, "/frame/"+unknown, nil)
	req.AddCookie(&http.Cookie{Name: studio.CookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
