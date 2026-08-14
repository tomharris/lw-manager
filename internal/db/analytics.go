package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	// roster stays data-in: it takes members and aliases as plain values and
	// must never import internal/db. Storage is allowed to depend on domain
	// logic; the reverse would make a db <-> roster cycle possible the moment
	// roster is given a query of its own.
	"github.com/tomharris/lw-manager/internal/roster"
)

// Alliance is the observed alliance. MemberCount is the "96/100" read off the
// alliance screen and is the roster route's reconciliation ground truth.
// ObservedAt is populated by CurrentAlliance (it is what "most recently
// observed" orders by) and left zero by UpsertAlliance's return, which
// reports only the id — a caller that needs the timestamp back reads it via
// CurrentAlliance instead of guessing at what `now()` resolved to server-side.
type Alliance struct {
	ID          int64
	Tag         string
	Name        string
	Server      string
	MemberCount int
	ObservedAt  time.Time
}

// Member is a dimension row: mutable and soft-deleted, unlike a fact.
type Member struct {
	ID             int64
	AllianceID     int64
	Name           string
	NameNormalized string
	Rank           string
	Active         bool
}

// Capture is one run of a collection route (e.g. vs_ranking, alliance
// roster) against one account. StartedAt is the collection timestamp ingest
// derives a replay-stable default period key from — never wall-clock now,
// so re-running ingest against an old capture months later reproduces the
// same period key it would have produced the day it was taken.
type Capture struct {
	ID           int64
	AccountID    int64
	Route        string
	StartedAt    time.Time
	Status       string
	ExpectedRows int
	ParsedRows   int
	Error        string
}

// CaptureFrame is one screenshot belonging to a capture, in scroll order.
type CaptureFrame struct {
	ID           int64
	CaptureID    int64
	Seq          int
	ScreenshotID int64
	OffsetPx     int
	GroupKey     string
}

// Fact is one append-only observation of a member metric. A correction is a
// new row that points the old one's superseded_by at it; nothing is ever
// updated in place, so every number still traces to the screenshot it came
// from.
type Fact struct {
	ID           int64
	MemberID     int64
	Metric       string
	Value        float64
	ObservedAt   time.Time
	PeriodKey    string
	Source       string
	ScreenshotID int64
	Confidence   float64
}

// ReviewItem is one row, header, or single field an OCR read could not
// confidently resolve (or write as a fact), queued for a human to confirm
// rather than guessed at. Confidence is the blended score that failed to
// clear the write-time gate — zero when the reason carries no such score
// (an unparseable field, an ambiguous name match, a full group), which is
// stored as SQL NULL rather than a claimed zero-confidence read.
type ReviewItem struct {
	ID           int64
	CaptureID    int64
	ScreenshotID int64
	RowY0, RowY1 int
	RawText      string
	Candidates   any
	Reason       string
	Confidence   float64
}

// UpsertAlliance creates or refreshes the observed alliance. Idempotent on
// (tag, name) so a re-run of the roster route corrects member_count rather
// than minting a duplicate alliance.
func (p *Pool) UpsertAlliance(ctx context.Context, a Alliance) (int64, error) {
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO alliances (tag, name, server, member_count, observed_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (tag, name) DO UPDATE
		  SET member_count = EXCLUDED.member_count, observed_at = now()
		RETURNING id`,
		a.Tag, a.Name, nullString(a.Server), a.MemberCount).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: upserting alliance %s/%s: %w", a.Tag, a.Name, err)
	}
	return id, nil
}

// SetAllianceMemberCount records the alliance's member_count from a fresh
// read of the alliance screen's own "Members: 96/100" line — the roster
// route's reconciliation ground truth (see Alliance's doc comment) — so the
// value persists rather than being used only transiently within one ingest
// run. It updates by id rather than going through UpsertAlliance's (tag,
// name) upsert: by the time ingest has an allianceID (from
// CurrentAllianceID), the alliances row already exists, and this call must
// not require re-supplying its tag and name just to refresh one column.
func (p *Pool) SetAllianceMemberCount(ctx context.Context, allianceID int64, count int) error {
	tag, err := p.Exec(ctx, `UPDATE alliances SET member_count = $2 WHERE id = $1`, allianceID, count)
	if err != nil {
		return fmt.Errorf("db: setting alliance %d member count: %w", allianceID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: alliance %d: %w", allianceID, ErrNotFound)
	}
	return nil
}

// CurrentAllianceID returns the id of the alliance most recently observed —
// this bot tracks exactly one alliance, so "most recently observed" is
// "the" alliance rather than a real disambiguation. Returns ErrNotFound
// before any `alliance` screen has ever been ingested into this table, which
// is the state of a fresh deployment.
func (p *Pool) CurrentAllianceID(ctx context.Context) (int64, error) {
	var id int64
	err := p.QueryRow(ctx, `SELECT id FROM alliances ORDER BY observed_at DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("db: current alliance: %w", ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("db: resolving current alliance: %w", err)
	}
	return id, nil
}

// CurrentAlliance returns the full row for the alliance most recently
// observed — the same "one alliance" resolution CurrentAllianceID performs
// for ingest, but with every column `control alliance show` needs to report
// rather than just the id. Returns ErrNotFound under the same condition
// CurrentAllianceID does: no `alliance` screen has ever been ingested, which
// on a fresh deployment means running `control alliance set` first, per the
// error control ingest now wraps with that instruction.
func (p *Pool) CurrentAlliance(ctx context.Context) (Alliance, error) {
	var a Alliance
	err := p.QueryRow(ctx, `
		SELECT id, tag, name, coalesce(server, ''), coalesce(member_count, 0), observed_at
		FROM alliances ORDER BY observed_at DESC LIMIT 1`).Scan(
		&a.ID, &a.Tag, &a.Name, &a.Server, &a.MemberCount, &a.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Alliance{}, fmt.Errorf("db: current alliance: %w", ErrNotFound)
	}
	if err != nil {
		return Alliance{}, fmt.Errorf("db: resolving current alliance: %w", err)
	}
	return a, nil
}

// Capture fetches one capture run by id.
func (p *Pool) Capture(ctx context.Context, id int64) (Capture, error) {
	var c Capture
	err := p.QueryRow(ctx, `
		SELECT id, account_id, route, started_at, status, coalesce(expected_rows, 0), coalesce(parsed_rows, 0), coalesce(error, '')
		FROM captures WHERE id = $1`, id).Scan(
		&c.ID, &c.AccountID, &c.Route, &c.StartedAt, &c.Status, &c.ExpectedRows, &c.ParsedRows, &c.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return Capture{}, fmt.Errorf("db: capture %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Capture{}, fmt.Errorf("db: reading capture %d: %w", id, err)
	}
	return c, nil
}

// ScreenshotObjectKey resolves a screenshot id to the blob key its bytes are
// stored under, which is all ingest needs — it verifies content by re-hashing
// what it reads, not by trusting a stored digest passed out of band.
func (p *Pool) ScreenshotObjectKey(ctx context.Context, screenshotID int64) (string, error) {
	var key string
	err := p.QueryRow(ctx, `SELECT object_key FROM screenshots WHERE id = $1`, screenshotID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("db: screenshot %d: %w", screenshotID, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("db: resolving object key for screenshot %d: %w", screenshotID, err)
	}
	return key, nil
}

// CreateCapture starts a new capture run and returns its id. Rows are
// visible immediately at status 'running', so a killed process leaves
// evidence rather than a silent gap.
func (p *Pool) CreateCapture(ctx context.Context, c Capture) (int64, error) {
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO captures (account_id, route, expected_rows)
		VALUES ($1, $2, $3)
		RETURNING id`,
		c.AccountID, c.Route, nullInt64(int64(c.ExpectedRows))).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: creating capture for account %d route %s: %w", c.AccountID, c.Route, err)
	}
	return id, nil
}

// AddCaptureFrame records one screenshot as belonging to a capture, in
// scroll order. offset_px is stored as measured rather than recomputed later,
// so a future change to the scroll offset calculation cannot silently
// re-segment historical captures.
func (p *Pool) AddCaptureFrame(ctx context.Context, f CaptureFrame) error {
	_, err := p.Exec(ctx, `
		INSERT INTO capture_frames (capture_id, seq, screenshot_id, offset_px, group_key)
		VALUES ($1, $2, $3, $4, $5)`,
		f.CaptureID, f.Seq, f.ScreenshotID, f.OffsetPx, nullString(f.GroupKey))
	if err != nil {
		return fmt.Errorf("db: adding frame %d to capture %d: %w", f.Seq, f.CaptureID, err)
	}
	return nil
}

// FinishCapture closes out a capture with its outcome. status is one of
// 'complete', 'partial' or 'failed' — 'partial' is load-bearing downstream,
// since a partial VS capture must not have its absences read as zeroes.
func (p *Pool) FinishCapture(ctx context.Context, id int64, status string, parsed int, errMsg string) error {
	tag, err := p.Exec(ctx, `
		UPDATE captures
		SET ended_at = now(), status = $2, parsed_rows = $3, error = $4
		WHERE id = $1`,
		id, status, parsed, nullString(errMsg))
	if err != nil {
		return fmt.Errorf("db: finishing capture %d as %q: %w", id, status, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: capture %d: %w", id, ErrNotFound)
	}
	return nil
}

// CaptureFrames returns every frame recorded for a capture, in scroll order.
func (p *Pool) CaptureFrames(ctx context.Context, captureID int64) ([]CaptureFrame, error) {
	rows, err := p.Query(ctx, `
		SELECT id, capture_id, seq, screenshot_id, offset_px, coalesce(group_key, '')
		FROM capture_frames
		WHERE capture_id = $1
		ORDER BY seq`, captureID)
	if err != nil {
		return nil, fmt.Errorf("db: listing frames for capture %d: %w", captureID, err)
	}
	defer rows.Close()

	var out []CaptureFrame
	for rows.Next() {
		var f CaptureFrame
		if err := rows.Scan(&f.ID, &f.CaptureID, &f.Seq, &f.ScreenshotID, &f.OffsetPx, &f.GroupKey); err != nil {
			return nil, fmt.Errorf("db: scanning frame for capture %d: %w", captureID, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CaptureFrameInput is one frame to persist as part of RecordCapture, before
// the capture row it belongs to exists — RecordCapture fills in that linkage
// itself once the row is inserted. It mirrors CaptureFrame's writable columns
// minus the capture linkage and the row's own id.
type CaptureFrameInput struct {
	ScreenshotID int64
	Seq          int
	OffsetPx     int
	GroupKey     string
}

// RecordCapture persists a capture and every one of its frames in a single
// transaction, so a killed process never leaves capture_frames rows pointing
// at a capture that does not exist. CreateCapture/AddCaptureFrame/
// FinishCapture above each hit the pool directly and were not meant to be
// composed for this: calling them in sequence would commit the capture row
// before its frames exist, which is exactly the partially-written state a
// kill must not produce.
//
// Status is derived here rather than accepted as free text: only a capture
// whose scroll proved the list bottom is 'complete'; anything else is
// 'partial'. That distinction is load-bearing downstream — the weekly VS
// ranking lists only members with a nonzero score, so ingest reads an absent
// member as a zero score only on a complete capture. Marking a capture
// complete when it did not prove the bottom would silently zero real scores.
//
// Frames are written with whatever Seq the caller supplies, unchanged.
// Multi-pass routes like roster_capture already renumber Seq sequentially
// across a whole run before calling this, so renumbering again here would
// silently discard the caller's ordering. offset_px is likewise stored
// exactly as given, never recomputed: a later change to vision.ScrollOffset
// must not re-segment a historical capture into different rows.
//
// parsed_rows and error are left NULL: this call records what was captured,
// not what OCR later makes of it, and that stage has not run yet.
func (p *Pool) RecordCapture(ctx context.Context, accountID int64, route string, frames []CaptureFrameInput, complete bool) error {
	status := "partial"
	if complete {
		status = "complete"
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: beginning capture %s for account %d: %w", route, accountID, err)
	}
	defer tx.Rollback(ctx)

	var captureID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO captures (account_id, route, status, ended_at)
		VALUES ($1, $2, $3, now())
		RETURNING id`,
		accountID, route, status).Scan(&captureID); err != nil {
		return fmt.Errorf("db: inserting capture %s for account %d: %w", route, accountID, err)
	}

	for _, f := range frames {
		if _, err := tx.Exec(ctx, `
			INSERT INTO capture_frames (capture_id, seq, screenshot_id, offset_px, group_key)
			VALUES ($1, $2, $3, $4, $5)`,
			captureID, f.Seq, f.ScreenshotID, f.OffsetPx, nullString(f.GroupKey)); err != nil {
			return fmt.Errorf("db: inserting frame seq %d of %s capture for account %d: %w", f.Seq, route, accountID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: committing %s capture for account %d: %w", route, accountID, err)
	}
	return nil
}

// ListMembers returns every active member of an alliance.
func (p *Pool) ListMembers(ctx context.Context, allianceID int64) ([]Member, error) {
	rows, err := p.Query(ctx, `
		SELECT id, alliance_id, name, name_normalized, coalesce(rank, ''), active
		FROM members
		WHERE alliance_id = $1 AND active
		ORDER BY name_normalized`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("db: listing members for alliance %d: %w", allianceID, err)
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.AllianceID, &m.Name, &m.NameNormalized, &m.Rank, &m.Active); err != nil {
			return nil, fmt.Errorf("db: scanning member for alliance %d: %w", allianceID, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateMember inserts a new member row. Members are a dimension, not a
// fact: a roster row that matches nothing existing mints a new member here
// rather than being guessed onto an existing one.
func (p *Pool) CreateMember(ctx context.Context, m Member) (int64, error) {
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO members (alliance_id, name, name_normalized, rank)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		m.AllianceID, m.Name, m.NameNormalized, nullString(m.Rank)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: creating member %q in alliance %d: %w", m.Name, m.AllianceID, err)
	}
	return id, nil
}

// MemberAliases returns every confirmed alias for every active member of an
// alliance, keyed by member id. Ingest folds this together with ListMembers
// into roster.Member, whose Aliases field is what makes a past human
// confirmation match directly next time instead of re-asking review.
func (p *Pool) MemberAliases(ctx context.Context, allianceID int64) (map[int64][]string, error) {
	rows, err := p.Query(ctx, `
		SELECT ma.member_id, ma.alias
		FROM member_aliases ma
		JOIN members m ON m.id = ma.member_id
		WHERE m.alliance_id = $1 AND m.active
		ORDER BY ma.member_id, ma.created_at`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("db: listing aliases for alliance %d: %w", allianceID, err)
	}
	defer rows.Close()

	out := make(map[int64][]string)
	for rows.Next() {
		var memberID int64
		var alias string
		if err := rows.Scan(&memberID, &alias); err != nil {
			return nil, fmt.Errorf("db: scanning alias for alliance %d: %w", allianceID, err)
		}
		out[memberID] = append(out[memberID], alias)
	}
	return out, rows.Err()
}

// AddAlias records a confirmed alternate spelling for a member. Every human
// confirmation writes one of these, which is what makes matching accuracy
// compound rather than needing to be re-tuned.
func (p *Pool) AddAlias(ctx context.Context, memberID int64, alias, source string) error {
	_, err := p.Exec(ctx, `
		INSERT INTO member_aliases (member_id, alias, alias_normalized, source)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (member_id, alias_normalized) DO NOTHING`,
		memberID, alias, roster.Normalize(alias), source)
	if err != nil {
		return fmt.Errorf("db: adding alias %q for member %d: %w", alias, memberID, err)
	}
	return nil
}

// InsertFact records one observation. Facts are append-only: a correction
// never updates an existing row, it inserts a new one and the caller points
// the old one's superseded_by at it via SupersedeFact.
func (p *Pool) InsertFact(ctx context.Context, f Fact) (int64, error) {
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO participation_facts
		  (member_id, metric, value, observed_at, period_key, source, screenshot_id, confidence)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		f.MemberID, f.Metric, f.Value, f.ObservedAt, f.PeriodKey, f.Source,
		nullInt64(f.ScreenshotID), f.Confidence).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: inserting %s fact for member %d in %s: %w", f.Metric, f.MemberID, f.PeriodKey, err)
	}
	return id, nil
}

// SupersedeFact points an old fact at its correction. Nothing is mutated in
// place beyond this pointer, so every number still traces to the screenshot
// it came from.
func (p *Pool) SupersedeFact(ctx context.Context, old, replacement int64) error {
	tag, err := p.Exec(ctx,
		`UPDATE participation_facts SET superseded_by = $2 WHERE id = $1`, old, replacement)
	if err != nil {
		return fmt.Errorf("db: superseding fact %d with %d: %w", old, replacement, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: fact %d: %w", old, ErrNotFound)
	}
	return nil
}

// LiveFacts returns the current (non-superseded) facts for a metric and
// period, highest value first.
func (p *Pool) LiveFacts(ctx context.Context, metric, periodKey string) ([]Fact, error) {
	rows, err := p.Query(ctx, `
		SELECT id, member_id, metric, value, observed_at, period_key, source,
		       coalesce(screenshot_id, 0), confidence
		FROM participation_facts
		WHERE metric = $1 AND period_key = $2 AND superseded_by IS NULL
		ORDER BY value DESC`, metric, periodKey)
	if err != nil {
		return nil, fmt.Errorf("db: listing live %s facts for %s: %w", metric, periodKey, err)
	}
	defer rows.Close()

	var out []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.ID, &f.MemberID, &f.Metric, &f.Value, &f.ObservedAt,
			&f.PeriodKey, &f.Source, &f.ScreenshotID, &f.Confidence); err != nil {
			return nil, fmt.Errorf("db: scanning fact: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// QueueReview files a row OCR could not confidently resolve, so it reaches a
// human instead of a leaderboard.
func (p *Pool) QueueReview(ctx context.Context, r ReviewItem) (int64, error) {
	blob, err := json.Marshal(r.Candidates)
	if err != nil {
		return 0, fmt.Errorf("db: encoding review candidates for screenshot %d: %w", r.ScreenshotID, err)
	}
	var id int64
	err = p.QueryRow(ctx, `
		INSERT INTO review_queue
		  (capture_id, screenshot_id, row_y0, row_y1, raw_text, candidates_json, reason, confidence)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		nullInt64(r.CaptureID), r.ScreenshotID, r.RowY0, r.RowY1, r.RawText, blob, r.Reason, nullFloat64(r.Confidence)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: queueing review for screenshot %d: %w", r.ScreenshotID, err)
	}
	return id, nil
}

// PendingReviews returns every review item still awaiting a human decision,
// oldest first -- the order a reviewer works down the queue in.
func (p *Pool) PendingReviews(ctx context.Context) ([]ReviewItem, error) {
	rows, err := p.Query(ctx, `
		SELECT id, coalesce(capture_id, 0), screenshot_id, row_y0, row_y1,
		       raw_text, candidates_json, reason, coalesce(confidence, 0)
		FROM review_queue
		WHERE status = 'pending'
		ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("db: listing pending reviews: %w", err)
	}
	defer rows.Close()
	return scanReviewItems(rows)
}

// Review fetches one review item by id, whatever its status -- a reviewer
// reloading the page for an item they just resolved (a stale tab, a double
// click) still needs it to load rather than 404, most concretely to fetch
// its crop.
func (p *Pool) Review(ctx context.Context, id int64) (ReviewItem, error) {
	rows, err := p.Query(ctx, `
		SELECT id, coalesce(capture_id, 0), screenshot_id, row_y0, row_y1,
		       raw_text, candidates_json, reason, coalesce(confidence, 0)
		FROM review_queue
		WHERE id = $1`, id)
	if err != nil {
		return ReviewItem{}, fmt.Errorf("db: reading review %d: %w", id, err)
	}
	defer rows.Close()
	items, err := scanReviewItems(rows)
	if err != nil {
		return ReviewItem{}, err
	}
	if len(items) == 0 {
		return ReviewItem{}, fmt.Errorf("db: review %d: %w", id, ErrNotFound)
	}
	return items[0], nil
}

// scanReviewItems decodes candidates_json back into []roster.Candidate for
// every row read, the reverse of the json.Marshal QueueReview does going in.
func scanReviewItems(rows pgx.Rows) ([]ReviewItem, error) {
	var out []ReviewItem
	for rows.Next() {
		var r ReviewItem
		var candidatesJSON []byte
		if err := rows.Scan(&r.ID, &r.CaptureID, &r.ScreenshotID, &r.RowY0, &r.RowY1,
			&r.RawText, &candidatesJSON, &r.Reason, &r.Confidence); err != nil {
			return nil, fmt.Errorf("db: scanning review item: %w", err)
		}
		var candidates []roster.Candidate
		if err := json.Unmarshal(candidatesJSON, &candidates); err != nil {
			return nil, fmt.Errorf("db: decoding candidates for review %d: %w", r.ID, err)
		}
		r.Candidates = candidates
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveReview marks a review item resolved and records the human's
// confirmation as a member alias -- the mechanism that makes matching
// compound, per CLAUDE.md: tomorrow's identical misread matches directly
// instead of queueing again. It returns the resolved item (mainly for its
// CaptureID) so a caller can point the reviewer at the right re-ingest
// command without a second lookup.
//
// It deliberately does not also write a participation fact, even though an
// early draft of this task's brief asked for exactly that. The project
// owner's ruling on the resulting defect report: the fact arrives by
// re-running ingest over the same capture. Resolving writes the alias;
// `control ingest --capture <id>` then matches that name directly (now that
// the alias exists) and writes the fact with the capture's own period_key
// and observed_at -- not today's, which a fact written here instead would
// get wrong. That replay property was repaired in Task 13 specifically so
// this works.
//
// Storing the row's already-OCR'd numeric value on review_queue, so a
// resolve here could write the fact immediately, was considered and
// rejected: it would put a second copy of the number outside
// participation_facts, and the fact would then be built from that stored
// copy rather than re-derived from the pixels on re-ingest -- weaker
// provenance than every other fact in the system, which invariant #5 ties
// to a screenshot, not to a cached intermediate value. So this method's
// contract is exactly two writes: the alias, and the status transition.
// Nothing else.
//
// The UPDATE's own "WHERE status = 'pending'" is the concurrency guard: a
// second resolve of the same id (a duplicate form submit, a slow response
// resubmitted) touches zero rows and reports ErrNotFound rather than writing
// a second alias.
func (p *Pool) ResolveReview(ctx context.Context, id, memberID int64, resolvedBy string) (ReviewItem, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return ReviewItem{}, fmt.Errorf("db: resolving review %d: %w", id, err)
	}
	defer tx.Rollback(ctx)

	item := ReviewItem{ID: id}
	err = tx.QueryRow(ctx, `
		UPDATE review_queue SET status = 'resolved', resolved_by = $2, resolved_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING coalesce(capture_id, 0), screenshot_id, row_y0, row_y1, raw_text, reason, coalesce(confidence, 0)`,
		id, resolvedBy).Scan(&item.CaptureID, &item.ScreenshotID, &item.RowY0, &item.RowY1,
		&item.RawText, &item.Reason, &item.Confidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewItem{}, fmt.Errorf("db: review %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return ReviewItem{}, fmt.Errorf("db: marking review %d resolved: %w", id, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO member_aliases (member_id, alias, alias_normalized, source)
		VALUES ($1, $2, $3, 'review')
		ON CONFLICT (member_id, alias_normalized) DO NOTHING`,
		memberID, item.RawText, roster.Normalize(item.RawText)); err != nil {
		return ReviewItem{}, fmt.Errorf("db: aliasing %q to member %d for review %d: %w", item.RawText, memberID, id, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ReviewItem{}, fmt.Errorf("db: committing resolution of review %d: %w", id, err)
	}
	return item, nil
}

// RejectReview marks a review item rejected: not any known member, or the
// read is unusable. It writes neither an alias nor a fact -- rejection is a
// statement that this row taught nothing, not a claim about anyone's
// identity.
func (p *Pool) RejectReview(ctx context.Context, id int64, resolvedBy string) error {
	tag, err := p.Exec(ctx, `
		UPDATE review_queue SET status = 'rejected', resolved_by = $2, resolved_at = now()
		WHERE id = $1 AND status = 'pending'`, id, resolvedBy)
	if err != nil {
		return fmt.Errorf("db: rejecting review %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: review %d: %w", id, ErrNotFound)
	}
	return nil
}

// nullString converts an empty string to SQL NULL, so an optional column
// stays NULL instead of storing "" when the caller has no value.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt64 converts a zero id to SQL NULL, for optional foreign keys where
// 0 means "no reference" rather than a real row id.
func nullInt64(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// nullFloat64 converts a zero confidence to SQL NULL: 0 means "no blended
// score applies to this review reason" (an unparseable field, an ambiguous
// name, a full group), never a claimed zero-confidence read.
func nullFloat64(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}
