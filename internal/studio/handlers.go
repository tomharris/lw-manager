package studio

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"image/png"
	"net/http"
	"net/url"
	"strconv"

	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

func (s *Server) handleUnsorted(w http.ResponseWriter, r *http.Request) {
	frames, err := s.corpus.List(corpus.Unsorted)
	if err != nil {
		s.fail(w, "listing unsorted frames", err)
		return
	}
	s.render(w, unsortedTmpl, map[string]any{
		"Title":      "unsorted",
		"Frames":     frames,
		"Labels":     KnownLabels,
		"CanCapture": s.tr != nil,
	})
}

func (s *Server) handleLabeled(w http.ResponseWriter, r *http.Request) {
	labels, err := s.corpus.Labels()
	if err != nil {
		s.fail(w, "listing labels", err)
		return
	}
	var groups []group
	for _, l := range labels {
		if l == corpus.Unsorted {
			continue
		}
		frames, err := s.corpus.List(l)
		if err != nil {
			s.fail(w, "listing frames for "+l, err)
			return
		}
		groups = append(groups, group{Label: l, Frames: frames})
	}
	s.render(w, labeledTmpl, map[string]any{
		"Title":      "labeled",
		"Groups":     groups,
		"Labels":     KnownLabels,
		"CanCapture": s.tr != nil,
	})
}

func (s *Server) handleLabel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	hash, label := r.FormValue("hash"), r.FormValue("label")

	// The label arrives from a browser form, so validate before it can become
	// a directory name.
	if err := corpus.CheckLabel(label); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch _, err := s.corpus.Relabel(hash, label); {
	case errors.Is(err, corpus.ErrNotFound):
		http.Error(w, "no such frame", http.StatusNotFound)
		return
	case err != nil:
		s.fail(w, "relabelling "+hash, err)
		return
	}
	http.Redirect(w, r, safeRedirectTarget(r), http.StatusSeeOther)
}

// safeRedirectTarget picks where to send the browser back to after a label,
// preferring the page it came from.
//
// This is an allowlist, not a validated echo of Referer, because a request
// header cannot be parsed the same way by this server and by the browser
// that will follow the Location it produces. Go's url.Parse follows
// RFC 3986, under which a backslash is not a path separator, so
// "/\evil.example/phish" parses with an empty Host and a literal-backslash
// Path — it looks like a safe relative path. Browsers follow the WHATWG URL
// spec instead, which normalizes "\" to "/" before resolving Location,
// turning that exact same string into "//evil.example/phish": a
// protocol-relative redirect straight off-site. Comparing parsed Host
// fields (the previous approach) cannot close that gap, because the two
// parsers disagree about what the string even is before comparison ever
// happens. The only safe move is to never use any part of Referer as the
// target: this studio has exactly two pages that carry a label form, "/"
// and "/labeled", so their paths are checked and a literal, server-chosen
// string is returned in every case — an absent Referer, an unparseable one,
// an off-host one, a backslash-bearing one, or anything else falls back
// to "/".
func safeRedirectTarget(r *http.Request) string {
	u, err := url.Parse(r.Header.Get("Referer"))
	if err != nil {
		return "/"
	}
	switch u.Path {
	case "/":
		return "/"
	case "/labeled":
		return "/labeled"
	default:
		return "/"
	}
}

// handleCapture grabs a frame from the device on demand. While cropping, the
// wanted screen is easier to produce on the handset than to hunt for in the
// corpus.
func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	if s.tr == nil {
		http.Error(w, "no device attached to this studio", http.StatusServiceUnavailable)
		return
	}
	img, err := s.tr.Screenshot(r.Context())
	if err != nil {
		s.fail(w, "capturing a frame", err)
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		s.fail(w, "encoding the captured frame", err)
		return
	}
	if _, _, err := s.corpus.Add(corpus.Unsorted, buf.Bytes()); err != nil {
		s.fail(w, "storing the captured frame", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleCropView(w http.ResponseWriter, r *http.Request) {
	f, err := s.corpus.Find(r.URL.Query().Get("hash"))
	if errors.Is(err, corpus.ErrNotFound) {
		http.Error(w, "no such frame", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "finding the frame to crop", err)
		return
	}
	s.render(w, cropTmpl, map[string]any{
		"Title":      "crop",
		"Frame":      f,
		"Labels":     KnownLabels,
		"CanCapture": s.tr != nil,
	})
}

func (s *Server) handleCrop(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	screen := r.FormValue("screen")
	anchorID := r.FormValue("anchor_id")

	// screen and anchor_id both become path segments under templates/, so
	// they are validated with the same rule that guards corpus labels.
	if err := corpus.CheckLabel(screen); err != nil {
		http.Error(w, "screen: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := corpus.CheckLabel(anchorID); err != nil {
		http.Error(w, "anchor_id: "+err.Error(), http.StatusBadRequest)
		return
	}

	region, err := rectFromForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	threshold, err := strconv.ParseFloat(r.FormValue("threshold"), 64)
	if err != nil {
		http.Error(w, "threshold: "+err.Error(), http.StatusBadRequest)
		return
	}

	data, err := s.corpus.Read(r.FormValue("hash"))
	if errors.Is(err, corpus.ErrNotFound) {
		http.Error(w, "no such frame", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "reading the frame to crop", err)
		return
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		http.Error(w, "frame is not a decodable PNG", http.StatusBadRequest)
		return
	}

	// The template is cut from this frame, so this frame's height is the
	// library's reference height. Two capture resolutions in one library
	// silently mis-scale every match.
	if h := img.Bounds().Dy(); h != s.refHeight {
		http.Error(w, fmt.Sprintf("frame is %dpx tall but the library reference height is %dpx",
			h, s.refHeight), http.StatusBadRequest)
		return
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, vision.Crop(img, region)); err != nil {
		s.fail(w, "encoding the cropped template", err)
		return
	}

	if err := vision.WriteAnchor(s.manifest, s.refHeight, vision.AnchorSpec{
		Screen:           screen,
		ID:               anchorID,
		Region:           region,
		Threshold:        threshold,
		IdentifiesScreen: r.FormValue("identifies_screen") != "",
	}, buf.Bytes()); err != nil {
		// WriteAnchor already rolled back, so a rejected crop leaves the
		// manifest exactly as it was. Report it as the user's problem.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/crop?hash="+r.FormValue("hash"), http.StatusSeeOther)
}

// rectFromForm reads the four normalized bounds the browser posted.
func rectFromForm(r *http.Request) (transport.Rect, error) {
	var vals [4]float64
	for i, name := range []string{"x1", "y1", "x2", "y2"} {
		v, err := strconv.ParseFloat(r.FormValue(name), 64)
		if err != nil {
			return transport.Rect{}, fmt.Errorf("%s: %w", name, err)
		}
		vals[i] = v
	}
	rect := transport.Rect{X1: vals[0], Y1: vals[1], X2: vals[2], Y2: vals[3]}
	if !rect.Valid() {
		return transport.Rect{}, fmt.Errorf("region %+v is not a valid unit-square rect", rect)
	}
	return rect, nil
}

func (s *Server) render(w http.ResponseWriter, t *template.Template, data any) {
	// Rendered into a buffer first so a template failure produces a 500
	// rather than a half-written page with a 200 already committed.
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		s.fail(w, "rendering", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.log.Warn("studio: writing response", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("studio: "+what, "err", err)
	http.Error(w, what+" failed", http.StatusInternalServerError)
}
