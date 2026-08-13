// Package studio serves the corpus labelling and cropping UI.
//
// It exists because the build host is headless and driven over SSH: a browser
// on another machine is the only surface where a screenshot can actually be
// looked at, so both labelling and cropping have to live there. Threshold
// tuning deliberately does not — that is a batch report over the whole corpus
// (see `agent score`), because the number worth having is computed across
// hundreds of frames rather than eyeballed on one.
package studio

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/transport"
)

// CookieName carries the studio token between requests.
const CookieName = "lw_studio"

// ErrTokenRequired reports a server constructed without a token.
var ErrTokenRequired = errors.New("studio: a token is required")

// Options configures a studio server.
type Options struct {
	Corpus *corpus.Store
	// Transport backs "capture now". Optional: without it the button is
	// disabled rather than the server refusing to start, so the studio is
	// still usable for labelling a corpus on a machine with no phone.
	Transport    transport.Transport
	ManifestPath string
	RefHeight    int
	Token        string
	Logger       *slog.Logger
	// Meta carries the capture-time metadata a "capture now" frame cannot
	// recover from its own bytes: device model and game version, the exact
	// fields the design doc names as the most likely explanation for a gate
	// that used to pass and now does not. CapturedAt is stamped fresh per
	// capture, not taken from this static value. Zero when Transport is nil.
	Meta corpus.Meta
	// Review, when set, registers the human review surface for uncertain OCR
	// reads (GET /review, GET /review/{id}/crop, POST /review/{id}/resolve,
	// POST /review/{id}/reject). Optional: corpus labelling is studio's
	// original job and needs no database, so a nil Review must leave that
	// path completely unaffected -- the review routes are then simply never
	// registered, rather than registered and erroring at request time.
	Review ReviewStore
	// Blobs backs GET /review/{id}/crop: a review item's screenshot lives in
	// the production blob store, resolved via ReviewStore.ScreenshotObjectKey,
	// never in the Corpus store above -- that is a separate, content-addressed
	// local directory built for labelling fixtures, and cannot serve a real
	// capture's screenshots. Required whenever Review is set.
	Blobs blob.Store
}

// Server serves the studio UI.
type Server struct {
	corpus    *corpus.Store
	tr        transport.Transport
	manifest  string
	refHeight int
	token     string
	log       *slog.Logger
	meta      corpus.Meta
	review    ReviewStore
	blobs     blob.Store
}

// New validates options and builds a server.
//
// The token is mandatory here rather than at the CLI. Binding to the LAN
// without auth is silent until it is not, and enforcing it at construction
// means no caller can forget.
func New(opts Options) (*Server, error) {
	if opts.Corpus == nil {
		return nil, fmt.Errorf("studio: a corpus store is required")
	}
	if opts.Token == "" {
		return nil, ErrTokenRequired
	}
	if opts.ManifestPath == "" {
		return nil, fmt.Errorf("studio: a manifest path is required")
	}
	if opts.RefHeight <= 0 {
		return nil, fmt.Errorf("studio: reference height must be positive, got %d", opts.RefHeight)
	}
	if opts.Review != nil && opts.Blobs == nil {
		return nil, fmt.Errorf("studio: a blob store is required when a review store is configured")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		corpus:    opts.Corpus,
		tr:        opts.Transport,
		manifest:  opts.ManifestPath,
		refHeight: opts.RefHeight,
		token:     opts.Token,
		log:       log,
		meta:      opts.Meta,
		review:    opts.Review,
		blobs:     opts.Blobs,
	}, nil
}

// RequireToken reports whether addr is non-loopback, and therefore must never
// be served without a token. A bare port or an empty host means every
// interface, which is the most exposed case, not the least.
func RequireToken(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true // not a literal IP recognised as loopback: assume exposed
	}
	return !ip.IsLoopback()
}

// Handler returns the routed, token-gated handler.
//
// Routes are registered by the task that implements them: Task 8 adds the
// label grid and capture routes, Task 9 the crop routes. Registering a route
// before its handler exists would mean shipping stubs, and a stub is
// indistinguishable from dead code to everyone who reads it later.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /frame/{hash}", s.handleFrame)
	mux.HandleFunc("GET /", s.handleUnsorted)
	mux.HandleFunc("GET /labeled", s.handleLabeled)
	mux.HandleFunc("POST /label", s.handleLabel)
	mux.HandleFunc("POST /capture", s.handleCapture)
	mux.HandleFunc("GET /crop", s.handleCropView)
	mux.HandleFunc("POST /crop", s.handleCrop)
	// Task 14: the review routes register only when a review store is
	// configured, so corpus-only use of studio (no database) is unaffected.
	if s.review != nil {
		mux.HandleFunc("GET /review", s.handleReviewList)
		mux.HandleFunc("GET /review/{id}/crop", s.handleReviewCrop)
		mux.HandleFunc("POST /review/{id}/resolve", s.handleReviewResolve)
		mux.HandleFunc("POST /review/{id}/reject", s.handleReviewReject)
	}
	return s.authenticate(mux)
}

// authenticate admits a request carrying the token in a cookie, or in ?t= on
// a first visit, in which case it sets the cookie and redirects to the same
// URL with the token stripped.
//
// The redirect matters: serving the request straight through would leave the
// token sitting in the address bar, browser history, and any outgoing
// Referer header, on every request that happens to carry ?t=. One redirect
// after the cookie lands closes that off.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t != "" && s.tokenOK(t) {
			http.SetCookie(w, &http.Cookie{
				Name:     CookieName,
				Value:    t,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, selfRedirectPath(r.URL), http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(CookieName)
		if err != nil || !s.tokenOK(c.Value) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// selfRedirectPath rebuilds the current request's path and query (with the
// token stripped) into a redirect target that can only ever point back at
// this host.
//
// It does not simply reflect u.RequestURI(): a GET request-target beginning
// with "//" (or, after browser-side "\"→"/" normalization, one beginning
// with "/\") parses on this server, under RFC 3986's request-target rules,
// as an ordinary path with an empty Host — the same bug class already fixed
// twice in handleLabel's safeRedirectTarget, and the same remedy applies:
// never echo a part of the request that a browser can parse differently
// than this server just did. Collapsing every leading '/' or '\' down to
// exactly one guarantees the Location this produces begins with a single
// "/", which no URL spec reads as anything but a same-host absolute path.
func selfRedirectPath(u *url.URL) string {
	p := u.Path
	i := 0
	for i < len(p) && (p[i] == '/' || p[i] == '\\') {
		i++
	}
	p = "/" + p[i:]

	q := u.Query()
	q.Del("t")
	if enc := q.Encode(); enc != "" {
		p += "?" + enc
	}
	return p
}

// tokenOK compares in constant time. The studio is on a LAN, not the open
// internet, but a timing-safe compare costs nothing here.
func (s *Server) tokenOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	data, err := s.corpus.Read(r.PathValue("hash"))
	// A malformed hash (e.g. a path-traversal attempt smuggled through the
	// {hash} wildcard) gets the same 404 as an absent one: a caller has no
	// use for knowing whether a request merely missed or was actually an
	// attempt to escape the corpus root.
	if errors.Is(err, corpus.ErrNotFound) || errors.Is(err, corpus.ErrInvalidHash) {
		http.Error(w, "no such frame", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("studio: reading frame", "hash", r.PathValue("hash"), "err", err)
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(data); err != nil {
		s.log.Warn("studio: writing frame response", "err", err)
	}
}
