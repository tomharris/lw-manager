package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tomharris/lw-manager/internal/roster"
)

// Alliance is the observed alliance. MemberCount is the "96/100" read off the
// alliance screen and is the roster route's reconciliation ground truth.
type Alliance struct {
	ID          int64
	Tag         string
	Name        string
	Server      string
	MemberCount int
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
// roster) against one account.
type Capture struct {
	ID           int64
	AccountID    int64
	Route        string
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

// ReviewItem is one row an OCR read could not confidently resolve into a
// member, queued for a human to confirm rather than guessed at.
type ReviewItem struct {
	ID           int64
	CaptureID    int64
	ScreenshotID int64
	RowY0, RowY1 int
	RawText      string
	Candidates   any
	Reason       string
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
		  (capture_id, screenshot_id, row_y0, row_y1, raw_text, candidates_json, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		nullInt64(r.CaptureID), r.ScreenshotID, r.RowY0, r.RowY1, r.RawText, blob, r.Reason).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: queueing review for screenshot %d: %w", r.ScreenshotID, err)
	}
	return id, nil
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
