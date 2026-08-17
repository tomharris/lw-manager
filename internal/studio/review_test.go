package studio

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/corpus"
	"github.com/tomharris/lw-manager/internal/db"
)

// fakeReviewStore is a wiring double: it proves the studio HTTP layer calls
// ReviewStore correctly (which method, with what arguments, in what order,
// and what it does with the returned item). It matches the real
// (Postgres-backed) *db.Pool.ResolveReview's contract exactly: resolving
// records an alias and returns the item (for its CaptureID), and nothing in
// this interface, real or fake, ever writes a participation fact -- that
// capability does not exist here at all. See ResolveReview's doc comment in
// internal/db/analytics.go for the project owner's ruling on why: the fact
// arrives later, by re-running ingest over the same capture.
type fakeReviewStore struct {
	Items       []pendingItem
	Screenshots map[int64]string

	Aliases  []fakeAlias
	Rejected []int64
}

type fakeAlias struct {
	ReviewID int64
	MemberID int64
}

func (f *fakeReviewStore) PendingReviews(ctx context.Context) ([]db.ReviewItem, error) {
	out := make([]db.ReviewItem, len(f.Items))
	for i, it := range f.Items {
		out[i] = toReviewItem(it)
	}
	return out, nil
}

func (f *fakeReviewStore) Review(ctx context.Context, id int64) (db.ReviewItem, error) {
	for _, it := range f.Items {
		if it.ID == id {
			return toReviewItem(it), nil
		}
	}
	return db.ReviewItem{}, fmt.Errorf("fakeReviewStore: review %d: %w", id, db.ErrNotFound)
}

func (f *fakeReviewStore) ScreenshotObjectKey(ctx context.Context, screenshotID int64) (string, error) {
	if key, ok := f.Screenshots[screenshotID]; ok {
		return key, nil
	}
	return "", fmt.Errorf("fakeReviewStore: screenshot %d: %w", screenshotID, db.ErrNotFound)
}

// ResolveReview records the alias and returns the resolved item, whether or
// not id was seeded in Items -- some tests exercise the handler against a
// store with nothing seeded at all, purely to check the id/memberID reach
// the store and the redirect follows. It never touches anything resembling
// a fact: there is no such call for it to make.
func (f *fakeReviewStore) ResolveReview(ctx context.Context, id, memberID int64, resolvedBy string) (db.ReviewItem, error) {
	f.Aliases = append(f.Aliases, fakeAlias{ReviewID: id, MemberID: memberID})
	for _, it := range f.Items {
		if it.ID == id {
			return toReviewItem(it), nil
		}
	}
	return db.ReviewItem{ID: id}, nil
}

func (f *fakeReviewStore) RejectReview(ctx context.Context, id int64, resolvedBy string) error {
	f.Rejected = append(f.Rejected, id)
	return nil
}

func toReviewItem(it pendingItem) db.ReviewItem {
	return db.ReviewItem{
		ID: it.ID, CaptureID: it.CaptureID, ScreenshotID: it.ScreenshotID, RowY0: it.RowY0, RowY1: it.RowY1,
		RawText: it.RawText, Candidates: it.Candidates, Reason: it.Reason, Confidence: it.Confidence,
	}
}

// authed carries the studio token as a cookie, following the same pattern as
// the black-box tests' own authed helper (handlers_test.go), just scoped to
// this file's package-internal tests.
func authed(req *http.Request) *http.Request {
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "s3cret"})
	return req
}

func newReviewServer(t *testing.T, items []pendingItem) *Server {
	t.Helper()
	return newReviewServerWithStore(t, &fakeReviewStore{Items: items})
}

func newReviewServerWithStore(t *testing.T, store *fakeReviewStore) *Server {
	t.Helper()
	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("blob.NewFSStore: %v", err)
	}
	s, err := New(Options{
		Corpus:       corpus.New(t.TempDir()),
		ManifestPath: t.TempDir() + "/manifest.yaml",
		RefHeight:    2400,
		Token:        "s3cret",
		Review:       store,
		Blobs:        blobs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestReviewListsPendingItemsWithCandidates(t *testing.T) {
	s := newReviewServer(t, []pendingItem{
		{ID: 1, RawText: "kaln445", Candidates: []candidate{{MemberID: 2, Name: "Kain445", Score: 86}}},
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/review", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "kaln445") || !strings.Contains(body, "Kain445") {
		t.Errorf("review page missing the raw text or the candidate: %q", body)
	}
}

// Resolving must write the alias, which is the mechanism that makes matching
// improve rather than needing to be re-tuned. It must not write a fact --
// the project owner's ruling on this task's original defect report confirmed
// that a fact only ever arrives by re-running ingest over the item's
// capture, never from the resolve call itself (see ResolveReview's doc
// comment in internal/db/analytics.go). This test's name and assertions
// originally claimed the opposite ("...AndTheFact", asserting store.Facts
// had one entry); that was the bug the defect report caught, and the
// assertion below is what replaced it.
func TestResolveWritesAnAliasButNoFact(t *testing.T) {
	store := &fakeReviewStore{}
	s := newReviewServerWithStore(t, store)

	req := httptest.NewRequest(http.MethodPost, "/review/1/resolve", strings.NewReader("member_id=2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(req))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if len(store.Aliases) != 1 || store.Aliases[0].ReviewID != 1 || store.Aliases[0].MemberID != 2 {
		t.Errorf("Aliases = %+v, want exactly one alias for review 1 / member 2", store.Aliases)
	}
}

// The redirect after a resolve is the only channel back to the reviewer, so
// it must carry enough for handleReviewList to render the "alias saved, run
// ingest over capture N" notice -- without it, a reviewer who just resolved
// an item has no way to learn the fact isn't written yet.
func TestResolveRedirectsWithAReingestNoticeNamingTheCapture(t *testing.T) {
	store := &fakeReviewStore{Items: []pendingItem{{ID: 5, CaptureID: 77, RawText: "kaln445"}}}
	s := newReviewServerWithStore(t, store)

	req := httptest.NewRequest(http.MethodPost, "/review/5/resolve", strings.NewReader("member_id=2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(req))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "resolved=5") || !strings.Contains(loc, "capture=77") {
		t.Fatalf("Location = %q, want it to carry the resolved id and capture id", loc)
	}

	// Follow the redirect, the way a browser would, and confirm the notice
	// itself names the capture and the ingest command.
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, authed(httptest.NewRequest(http.MethodGet, loc, nil)))
	body := rec2.Body.String()
	if !strings.Contains(body, "control ingest --capture 77") {
		t.Fatalf("review page after resolving did not name the re-ingest command: %q", body)
	}
}

func TestReviewRequiresTheToken(t *testing.T) {
	s := newReviewServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/review", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("unauthenticated access to /review must not succeed")
	}
}

// Rejecting an item (not any known member, or the read is unusable) must
// mark it without writing an alias or a fact for anyone.
func TestRejectWritesNeitherAliasNorFactButMarksTheItem(t *testing.T) {
	store := &fakeReviewStore{Items: []pendingItem{{ID: 7, RawText: "garbled"}}}
	s := newReviewServerWithStore(t, store)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(httptest.NewRequest(http.MethodPost, "/review/7/reject", nil)))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if len(store.Aliases) != 0 {
		t.Errorf("aliases written = %d, want 0", len(store.Aliases))
	}
	if len(store.Rejected) != 1 || store.Rejected[0] != 7 {
		t.Errorf("Rejected = %v, want [7]", store.Rejected)
	}
}

// The crop endpoint must serve exactly the [row_y0, row_y1) band of the
// screenshot the review item points at -- the whole reason this is a served
// UI rather than a CLI is that the reviewer needs to see those pixels.
func TestReviewCropServesTheStoredRowBand(t *testing.T) {
	const w, h = 100, 200
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding fixture PNG: %v", err)
	}

	blobs, err := blob.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("blob.NewFSStore: %v", err)
	}
	key, _, err := blob.PutContent(context.Background(), blobs, buf.Bytes())
	if err != nil {
		t.Fatalf("PutContent: %v", err)
	}

	store := &fakeReviewStore{
		Items:       []pendingItem{{ID: 3, ScreenshotID: 42, RowY0: 40, RowY1: 100}},
		Screenshots: map[int64]string{42: key},
	}
	s, err := New(Options{
		Corpus:       corpus.New(t.TempDir()),
		ManifestPath: t.TempDir() + "/manifest.yaml",
		RefHeight:    2400,
		Token:        "s3cret",
		Review:       store,
		Blobs:        blobs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/review/3/crop", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
	cropped, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decoding crop response: %v", err)
	}
	if got := cropped.Bounds().Dx(); got != w {
		t.Errorf("crop width = %d, want the full frame width %d", got, w)
	}
	if got := cropped.Bounds().Dy(); got != 60 {
		t.Errorf("crop height = %d, want row_y1 - row_y0 = 60", got)
	}
	// The crop is a view onto the original band, not a re-origined copy: the
	// pixel at the crop's own (0,0) is the source frame's (0, row_y0).
	wantR, wantG, _, _ := img.At(0, 40).RGBA()
	gotR, gotG, _, _ := cropped.At(0, 0).RGBA()
	if wantR != gotR || wantG != gotG {
		t.Errorf("crop origin pixel = (%d,%d), want the frame's (0,40) pixel (%d,%d)", gotR, gotG, wantR, wantG)
	}
}

// Studio's original job -- corpus labelling -- needs no database. A studio
// built with no Review store must still serve the corpus routes, and must
// never dispatch into the review handlers at all (they would nil-pointer on
// s.review otherwise).
//
// GET /review is asserted as a 404, not a fallback to the corpus page. An
// earlier version of this test accepted a 200 there, reasoning that Go's
// ServeMux would route it through the "GET /" catch-all into handleUnsorted
// the same as any other unmatched path (e.g. "/nope" in studio_test.go's own
// routing tests) -- true of an arbitrary path, but /review is this task's
// own advertised interface: an operator hitting it expecting the review
// queue and silently getting the corpus page instead has no signal that
// review is off. Handler() now registers "GET /review" explicitly in the
// no-store case too, ahead of the catch-all, to return 404 with a reason.
func TestStudioWithoutAReviewStoreStillServesCorpusRoutesAndOmitsReview(t *testing.T) {
	store := corpus.New(t.TempDir())
	f, _, err := store.Add(corpus.Unsorted, []byte("frame-bytes"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	s, err := New(Options{
		Corpus:       store,
		ManifestPath: t.TempDir() + "/manifest.yaml",
		RefHeight:    2400,
		Token:        "s3cret",
		// No Review, no Blobs: exactly the corpus-only construction studio
		// has always supported.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("corpus route: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), f.Hash) {
		t.Fatal("corpus route did not list the seeded frame")
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(httptest.NewRequest(http.MethodGet, "/review", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /review with no review store: status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "review unavailable") {
		t.Fatalf("GET /review 404 body = %q, want it to name the reason", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, authed(httptest.NewRequest(http.MethodPost, "/review/1/resolve", nil)))
	if rec.Code == http.StatusOK || rec.Code == http.StatusSeeOther {
		t.Fatalf("POST /review/1/resolve with no review store: status = %d, want it to fail rather than resolve anything", rec.Code)
	}
}
