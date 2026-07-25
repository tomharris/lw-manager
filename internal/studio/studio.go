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
	"strings"

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
}

// Server serves the studio UI.
type Server struct {
	corpus    *corpus.Store
	tr        transport.Transport
	manifest  string
	refHeight int
	token     string
	log       *slog.Logger
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
		return true // a hostname we cannot resolve to loopback: assume exposed
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
	return s.authenticate(mux)
}

// authenticate admits a request carrying the token in a cookie, or in ?t= on
// a first visit, in which case it sets the cookie so later requests carry it.
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
			next.ServeHTTP(w, r)
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

// tokenOK compares in constant time. The studio is on a LAN, not the open
// internet, but a timing-safe compare costs nothing here.
func (s *Server) tokenOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	data, err := s.corpus.Read(r.PathValue("hash"))
	if errors.Is(err, corpus.ErrNotFound) {
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
