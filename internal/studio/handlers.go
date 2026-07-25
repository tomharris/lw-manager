package studio

import (
	"bytes"
	"errors"
	"html/template"
	"image/png"
	"net/http"

	"github.com/tomharris/lw-manager/internal/corpus"
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
	http.Redirect(w, r, r.Header.Get("Referer")+"", http.StatusSeeOther)
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
