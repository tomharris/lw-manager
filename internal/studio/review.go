// Review handlers: the human review surface for uncertain OCR reads. See
// package studio's doc comment for why this lives in the same server as
// corpus labelling rather than a second one -- both need a browser over SSH,
// auth, and a way to serve a crop of the actual pixels beside a decision.
package studio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/roster"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// candidate is roster.Candidate under the name the review templates use.
type candidate = roster.Candidate

// pendingItem is one review_queue row as the review page renders it.
type pendingItem struct {
	ID           int64
	CaptureID    int64
	ScreenshotID int64
	RowY0, RowY1 int
	RawText      string
	Candidates   []candidate
	Reason       string
	Confidence   float64
}

// ReviewStore is the database surface the review UI needs: list what is
// pending, look one item up (for its crop), resolve one to a member, or
// reject one outright. *db.Pool satisfies it once the small methods it is
// missing exist on it -- added alongside this task in
// internal/db/analytics.go, in the same hand-written pgx style as the rest
// of that file.
//
// ResolveReview returns the resolved item (mainly for its CaptureID) so the
// handler can tell the reviewer which capture to re-ingest without a second
// round trip -- see handleReviewResolve.
type ReviewStore interface {
	PendingReviews(ctx context.Context) ([]db.ReviewItem, error)
	Review(ctx context.Context, id int64) (db.ReviewItem, error)
	ScreenshotObjectKey(ctx context.Context, screenshotID int64) (string, error)
	ResolveReview(ctx context.Context, id, memberID int64, resolvedBy string) (db.ReviewItem, error)
	RejectReview(ctx context.Context, id int64, resolvedBy string) error
}

// resolvedBySource stands in for a reviewer's identity in resolved_by.
// Studio authenticates with one shared token, not per-user accounts, so
// there is no real identity to record -- this just marks the source as
// studio, the same spirit as QueueReview's own "ocr:alliance_members" /
// "manual" source strings.
const resolvedBySource = "studio"

// toPendingItem adapts a stored review row for the template. Candidates
// comes back from ReviewStore as `any` holding []roster.Candidate (see
// db.scanReviewItems); a review reason with no candidates (a field parse
// failure, not a name match) leaves it nil, which the template renders as an
// empty candidate list rather than failing.
func toPendingItem(r db.ReviewItem) pendingItem {
	cands, _ := r.Candidates.([]roster.Candidate)
	return pendingItem{
		ID:           r.ID,
		CaptureID:    r.CaptureID,
		ScreenshotID: r.ScreenshotID,
		RowY0:        r.RowY0,
		RowY1:        r.RowY1,
		RawText:      r.RawText,
		Candidates:   cands,
		Reason:       r.Reason,
		Confidence:   r.Confidence,
	}
}

func (s *Server) handleReviewList(w http.ResponseWriter, r *http.Request) {
	items, err := s.review.PendingReviews(r.Context())
	if err != nil {
		s.fail(w, "listing pending reviews", err)
		return
	}
	pending := make([]pendingItem, len(items))
	for i, it := range items {
		pending[i] = toPendingItem(it)
	}
	s.render(w, reviewTmpl, map[string]any{
		"Title":      "review",
		"Items":      pending,
		"CanCapture": s.tr != nil,
		"Notice":     resolveNotice(r.URL.Query()),
	})
}

// resolveNotice turns handleReviewResolve's redirect query params into the
// confirmation a reviewer needs to see. Resolving only ever writes an alias
// (ReviewStore.ResolveReview's doc comment, and the one on the underlying
// db.Pool.ResolveReview, explain why) -- the fact does not exist until
// ingest re-runs over the originating capture and re-derives it from the
// pixels. Without this banner a reviewer who just resolved an item has no
// way to learn that, and the natural (wrong) conclusion is that the work is
// done; a 303 redirect is the only channel back from the resolve POST, so
// the confirmation has to travel as query params.
func resolveNotice(q url.Values) string {
	id := q.Get("resolved")
	if id == "" {
		return ""
	}
	capture := q.Get("capture")
	if capture == "" {
		return fmt.Sprintf("Review #%s resolved: the alias is saved. Its capture id was not recorded on this row, so find the capture that produced it and re-run ingest over that to write the fact.", id)
	}
	return fmt.Sprintf("Review #%s resolved: the alias is saved. Run `control ingest --capture %s` to write the fact.", id, capture)
}

// handleReviewCrop serves the row crop: the screenshot the review item came
// from, cut to [row_y0, row_y1) at full width. It reuses handleCrop's own
// decode-then-vision.Crop path rather than a new one -- the only difference
// is where the bytes come from, the production blob store resolved via
// ScreenshotObjectKey, not the corpus's content-addressed local directory.
func (s *Server) handleReviewCrop(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad review id", http.StatusBadRequest)
		return
	}
	item, err := s.review.Review(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "no such review item", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "finding review "+strconv.FormatInt(id, 10), err)
		return
	}

	key, err := s.review.ScreenshotObjectKey(r.Context(), item.ScreenshotID)
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "no such screenshot", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, "resolving screenshot for review "+strconv.FormatInt(id, 10), err)
		return
	}

	rc, err := s.blobs.Get(r.Context(), key)
	if err != nil {
		s.fail(w, "reading screenshot blob", err)
		return
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		s.fail(w, "reading screenshot bytes", err)
		return
	}

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		s.fail(w, "decoding screenshot", err)
		return
	}

	h := img.Bounds().Dy()
	if h <= 0 {
		http.Error(w, "screenshot has no height", http.StatusInternalServerError)
		return
	}
	region := transport.Rect{X1: 0, X2: 1,
		Y1: clamp01(float64(item.RowY0) / float64(h)),
		Y2: clamp01(float64(item.RowY1) / float64(h)),
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, vision.Crop(img, region)); err != nil {
		s.fail(w, "encoding the row crop", err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.log.Warn("studio: writing review crop response", "err", err)
	}
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// handleReviewResolve records the reviewer's pick. ReviewStore.ResolveReview
// writes the alias now -- the mechanism that makes matching compound, so
// tomorrow's identical misread matches directly instead of queueing again --
// and stops there. It never writes a participation fact: the fact arrives
// later, by re-running ingest over the same capture (`control ingest
// --capture <id>`), which then matches the newly-aliased name directly and
// writes the fact with the capture's own period_key and observed_at, not
// today's. That replay property is what Task 13 repaired specifically so
// this works. See ResolveReview's doc comment in internal/db/analytics.go
// for the project owner's ruling on why the row's numeric value is not
// stored here to shortcut that: it would be a second copy of the number
// living outside participation_facts, and the fact would be built from that
// copy instead of re-derived from the pixels -- weaker provenance than every
// other fact in the system.
//
// Because a 303 redirect is the only channel back to the reviewer, the
// resolved id and (when known) the capture id travel as query params for
// handleReviewList to turn into the on-page notice -- without it, a
// reviewer has no way to learn the loop isn't finished yet.
func (s *Server) handleReviewResolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad review id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	memberID, err := strconv.ParseInt(r.FormValue("member_id"), 10, 64)
	if err != nil {
		http.Error(w, "member_id: "+err.Error(), http.StatusBadRequest)
		return
	}

	item, err := s.review.ResolveReview(r.Context(), id, memberID, resolvedBySource)
	switch {
	case errors.Is(err, db.ErrNotFound):
		http.Error(w, "no such pending review", http.StatusNotFound)
		return
	case err != nil:
		s.fail(w, "resolving review "+strconv.FormatInt(id, 10), err)
		return
	}

	redirect := url.URL{Path: "/review", RawQuery: url.Values{"resolved": {strconv.FormatInt(id, 10)}}.Encode()}
	if item.CaptureID != 0 {
		q := redirect.Query()
		q.Set("capture", strconv.FormatInt(item.CaptureID, 10))
		redirect.RawQuery = q.Encode()
	}
	http.Redirect(w, r, redirect.String(), http.StatusSeeOther)
}

// handleReviewReject marks an item rejected: not any known member, or the
// read is unusable. It writes neither an alias nor a fact.
func (s *Server) handleReviewReject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad review id", http.StatusBadRequest)
		return
	}
	switch err := s.review.RejectReview(r.Context(), id, resolvedBySource); {
	case errors.Is(err, db.ErrNotFound):
		http.Error(w, "no such pending review", http.StatusNotFound)
		return
	case err != nil:
		s.fail(w, "rejecting review "+strconv.FormatInt(id, 10), err)
		return
	}
	http.Redirect(w, r, "/review", http.StatusSeeOther)
}
