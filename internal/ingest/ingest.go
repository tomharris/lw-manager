package ingest

import (
	"context"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/ocr"
)

// Store is the database surface both ingest routes (roster, VS) need. It is
// an interface so tests run against a fake with no Postgres, per the
// replay-before-real discipline this project follows everywhere — *db.Pool
// satisfies it.
//
// It is deliberately not named for one route: CreateMember/InsertFact/
// QueueReview/FinishCapture are shared by roster and VS ingest alike, and
// splitting them per caller would just mean two fakes that both wrap the
// same *db.Pool methods.
type Store interface {
	Capture(ctx context.Context, id int64) (db.Capture, error)
	CaptureFrames(ctx context.Context, captureID int64) ([]db.CaptureFrame, error)
	ScreenshotObjectKey(ctx context.Context, screenshotID int64) (string, error)
	ListMembers(ctx context.Context, allianceID int64) ([]db.Member, error)
	MemberAliases(ctx context.Context, allianceID int64) (map[int64][]string, error)
	CreateMember(ctx context.Context, m db.Member) (int64, error)
	InsertFact(ctx context.Context, f db.Fact) (int64, error)
	// UpsertFact is InsertFact's idempotent sibling — see its own doc
	// comment in internal/db/analytics.go. Only the roster route uses it
	// today (task 27): observed_at is pinned to the capture's started_at
	// for every roster frame, so a repeat read of the same member/metric
	// within one capture — or across a re-run of a crashed one — hits the
	// exact same key InsertFact would reject with a unique-constraint
	// error. VS ingest has no equivalent within-capture collision (each
	// row is read once) and keeps using InsertFact unchanged.
	UpsertFact(ctx context.Context, f db.Fact) (id int64, written bool, err error)
	QueueReview(ctx context.Context, r db.ReviewItem) (int64, error)
	FinishCapture(ctx context.Context, id int64, status string, parsed int, errMsg string) error
	CurrentAllianceID(ctx context.Context) (int64, error)
	SetAllianceMemberCount(ctx context.Context, allianceID int64, count int) error
}

// Ingester turns stored capture frames into members and facts. It never
// touches a device — everything it does starts from a screenshot already
// sitting in the blob store, which is what keeps this package's tests
// device-free and lets a parser fix be replayed over every capture ever
// taken.
type Ingester struct {
	store  Store
	blobs  blob.Store
	engine ocr.OCREngine
}

// New builds an Ingester over the given data store, blob store and OCR
// engine. Tests construct one over fakes of all three; production wires
// *db.Pool, the real blob backend and ocr.NewTesseractEngine().
func New(store Store, blobs blob.Store, engine ocr.OCREngine) *Ingester {
	return &Ingester{store: store, blobs: blobs, engine: engine}
}
