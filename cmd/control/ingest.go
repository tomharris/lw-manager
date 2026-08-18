package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/ingest"
	"github.com/tomharris/lw-manager/internal/ocr"
)

// ingester is the surface runIngest needs: the capture row (to dispatch on
// its route and derive the default period key) and the two route
// implementations. It is deliberately one interface rather than two so a
// single fake can drive every runIngest test without a database — production
// satisfies it with ingestService, which fans the calls out to a *db.Pool
// and an *ingest.Ingester.
type ingester interface {
	Capture(ctx context.Context, id int64) (db.Capture, error)
	IngestRoster(ctx context.Context, captureID int64, periodKey string) (ingest.RosterResult, error)
	IngestVS(ctx context.Context, captureID int64, periodKey string) (ingest.VSResult, error)
}

// ingestService adapts a *db.Pool and an *ingest.Ingester — two different
// types, per internal/ingest/store.go's CaptureStore-vs-Store split — to the
// single ingester interface runIngest depends on.
type ingestService struct {
	pool *db.Pool
	ing  *ingest.Ingester
}

func (s ingestService) Capture(ctx context.Context, id int64) (db.Capture, error) {
	return s.pool.Capture(ctx, id)
}

func (s ingestService) IngestRoster(ctx context.Context, captureID int64, periodKey string) (ingest.RosterResult, error) {
	return s.ing.IngestRoster(ctx, captureID, periodKey)
}

func (s ingestService) IngestVS(ctx context.Context, captureID int64, periodKey string) (ingest.VSResult, error) {
	return s.ing.IngestVS(ctx, captureID, periodKey)
}

// runIngestCmd wires the real dependencies and runs the ingest subcommand.
// It is the only piece of this file that touches Postgres, blob storage or
// Tesseract — everything else is exercised in cmd/control/ingest_test.go
// against a fake, with no Docker required.
func runIngestCmd(ctx context.Context, cfg config.Config, args []string) error {
	// Checked first, before Postgres or blob storage are even touched: a
	// missing tesseract binary is knowable instantly and does not depend on
	// anything this command is about to connect to. Discovering it instead
	// three wraps down, after a roster capture has already ingested most of
	// its groups, is the exact failure Task 20 was filed against.
	engine := ocr.NewTesseractEngine()
	if !preflightOCR(os.Stderr, engine) {
		return fmt.Errorf("control ingest: failed")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		return err
	}

	svc := ingestService{pool: pool, ing: ingest.New(pool, blobs, engine)}

	if code := runIngest(os.Stdout, os.Stderr, args, svc); code != 0 {
		return fmt.Errorf("control ingest: failed")
	}
	return nil
}

// availabilityChecker is the optional capability preflightOCR looks for on
// an ocr.OCREngine. It is deliberately not part of the OCREngine interface
// itself: OCREngine is also implemented by ocr.FakeEngine, the device- and
// tesseract-free stand-in every ingest test depends on, and a fake replaying
// scripted results has no meaningful answer to "is your subprocess on
// PATH". Requiring Available() on OCREngine would force the fake to either
// fabricate an answer or refuse to compile — making the assertion optional
// is what lets an engine that cannot report availability simply skip the
// check instead of being rejected by it.
type availabilityChecker interface {
	Available() bool
}

// preflightOCR refuses before any ingest work starts if engine both can and
// does report itself unavailable. This is the whole fix: the same
// information TesseractEngine.Available() already carried, surfaced before
// the run rather than after the first roster group fails to OCR and the
// cause is buried three levels down a wrapped error.
func preflightOCR(errOut io.Writer, engine ocr.OCREngine) bool {
	checker, ok := engine.(availabilityChecker)
	if !ok {
		return true
	}
	if checker.Available() {
		return true
	}
	fmt.Fprintln(errOut, "control ingest: tesseract is not available on PATH; install it with `apt install tesseract-ocr tesseract-ocr-eng` (Debian/Ubuntu) and re-run")
	return false
}

// runIngest parses ingest's flags and runs the route the capture row names.
// Results are written to out (stdout in production) and everything else —
// usage errors, failures — to errOut (stderr), so a caller can pipe the
// summary without log lines mixed in. It returns a process exit code rather
// than an error so cmd/control/ingest_test.go can assert on both the code
// and which writer got what, without a database.
func runIngest(out, errOut io.Writer, args []string, svc ingester) int {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(errOut)
	captureID := fs.Int64("capture", 0, "capture id to ingest (required)")
	period := fs.String("period", "", "period key override (defaults from the capture's own started_at: ISO week for vs_ranking, date for roster)")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "usage: control ingest --capture <id> [--period <key>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *captureID == 0 {
		fmt.Fprintln(errOut, "control ingest: --capture is required")
		fs.Usage()
		return 1
	}

	// Scoped to errOut rather than the global slog logger: it keeps this
	// command's log output going to the writer the caller actually gave it
	// (real stderr in production, a test buffer under test), instead of
	// depending on main()'s logging.Setup having already run.
	logger := slog.New(slog.NewTextHandler(errOut, nil))
	ctx := context.Background()

	capture, err := svc.Capture(ctx, *captureID)
	if err != nil {
		logger.ErrorContext(ctx, "control ingest: loading capture failed", "capture_id", *captureID, "error", err)
		return 1
	}

	periodKey := *period
	if periodKey == "" {
		periodKey = defaultPeriodKey(capture.Route, capture.StartedAt)
	}

	switch capture.Route {
	case "roster":
		res, err := svc.IngestRoster(ctx, *captureID, periodKey)
		if err != nil {
			logger.ErrorContext(ctx, "control ingest: roster ingest failed", "capture_id", *captureID, "error", err)
			return 1
		}
		printRosterSummary(out, *captureID, periodKey, res)
		return 0

	case "vs_ranking":
		res, err := svc.IngestVS(ctx, *captureID, periodKey)
		if err != nil {
			logger.ErrorContext(ctx, "control ingest: vs ranking ingest failed", "capture_id", *captureID, "error", err)
			return 1
		}
		printVSSummary(out, *captureID, periodKey, res)
		return 0

	default:
		fmt.Fprintf(errOut, "control ingest: capture %d: unknown route %q\n", *captureID, capture.Route)
		return 1
	}
}

// defaultPeriodKey derives a period key from the capture's own started_at —
// never from wall-clock now — so re-running ingest months later against an
// old capture reproduces the same key it would have produced on the day it
// was taken, which is what makes replay meaningful at all. vs_ranking's
// weekly ranking screen is keyed to an ISO week ("2026-W33"); roster has no
// weekly grain of its own, so it keys to the calendar date.
func defaultPeriodKey(route string, startedAt time.Time) string {
	if route == "vs_ranking" {
		year, week := startedAt.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	}
	return startedAt.Format("2006-01-02")
}

// printRosterSummary names what happened clearly enough that a human can
// tell a clean run from one that quietly reviewed half the roster: matched,
// created, queued, the final capture status, and each rank group's own
// reconciliation tally, sorted for a deterministic report.
func printRosterSummary(out io.Writer, captureID int64, periodKey string, res ingest.RosterResult) {
	fmt.Fprintf(out, "capture=%d route=roster period=%s matched=%d created=%d queued=%d status=%s\n",
		captureID, periodKey, res.Matched, res.Created, res.Queued, res.Status)

	// alliance_checked=false means the alliance frame was missing from the
	// capture or its "Members: X/Y" read failed — the roster still ingested
	// on per-group reconciliation alone (see internal/ingest/roster.go), and
	// alliance_members is meaningless in that case, so it is omitted rather
	// than printed as a misleading 0.
	if res.AllianceTotalChecked {
		fmt.Fprintf(out, "  alliance_members=%d alliance_checked=true\n", res.AllianceMemberCount)
	} else {
		fmt.Fprintln(out, "  alliance_checked=false")
	}

	keys := make([]string, 0, len(res.PerGroup))
	for k := range res.PerGroup {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := res.PerGroup[k]
		// Name is printed only when present: a capture whose header never
		// parsed a name for this rank (every frame queued to review before
		// GroupTally.Name could be set) must not print a misleading empty
		// quoted string.
		if t.Name != "" {
			fmt.Fprintf(out, "  group=%s name=%q parsed=%d expected=%d\n", k, t.Name, t.Parsed, t.Expected)
		} else {
			fmt.Fprintf(out, "  group=%s parsed=%d expected=%d\n", k, t.Parsed, t.Expected)
		}
	}
}

// printVSSummary mirrors printRosterSummary for the VS route: matched,
// queued, zeroed (VS-only — the roster route never infers a fact), and the
// final capture status.
//
// unidentified is printed alongside because it is what makes zeroed readable.
// Zero inference is suppressed entirely while any row remains unattributed
// (see internal/ingest/vs.go), so "zeroed=0" means either that nobody was
// absent or that the run declined to guess who was — and only unidentified
// tells the two apart. The follow-up line names the action, since deferred
// zeroes are recovered by clearing the queue and ingesting the same capture
// again, not by re-capturing.
func printVSSummary(out io.Writer, captureID int64, periodKey string, res ingest.VSResult) {
	fmt.Fprintf(out, "capture=%d route=vs_ranking period=%s matched=%d queued=%d zeroed=%d unidentified=%d duplicates=%d status=%s\n",
		captureID, periodKey, res.Matched, res.Queued, res.Zeroed, res.Unidentified, res.Duplicates, res.Status)

	if res.Unidentified > 0 && res.Status == "complete" {
		fmt.Fprintf(out, "  no zeroes inferred: %d rows could not be attributed to a member, so absence is not proof of a zero score.\n", res.Unidentified)
		fmt.Fprintf(out, "  resolve them in `agent studio` and re-run `control ingest --capture %d`.\n", captureID)
	}
}
