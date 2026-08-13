package ingest

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/ocr"
	"github.com/tomharris/lw-manager/internal/roster"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// memberListRegion and memberRowPitch mirror tasks.memberListRegion and
// tasks.memberRowPitch (measured on the handset — see internal/tasks/
// roster_capture.go). They are duplicated rather than imported: internal/
// tasks depends on internal/runtime, which this device-free package must
// not pull in, and capture_frames.offset_px was measured against exactly
// this region, so ingest re-measuring rows against a different one would
// silently misalign every row (see the package doc on SegmentRows/offset_px
// in the design). If the capture-side region ever moves, this constant must
// move with it.
var memberListRegion = transport.Rect{X1: 0.03, Y1: 0.42, X2: 0.97, Y2: 0.89}

const memberRowPitch = 112

// groupHeaderRegion is the band at the top of memberListRegion the sticky
// rank-group header occupies once it has pinned there mid-scroll. Height is
// recon-measured (~55px of a 1600px-tall frame, frame 03 in
// docs/superpowers/specs/evidence/m4-recon-2026-08-12/) with margin, and is
// UNVERIFIED on a live scroll session — the recon frame it came from was
// captured immediately after the group's chevron was tapped, before any
// scrolling pinned the header to this exact position. Owed to
// `make test-device` alongside the rest of this route's hardware
// verification (see progress.md Task 8b).
var groupHeaderRegion = transport.Rect{
	X1: memberListRegion.X1, X2: memberListRegion.X2,
	Y1: memberListRegion.Y1, Y2: memberListRegion.Y1 + 0.05,
}

// Field sub-rects, as fractions of the full frame width (X) and of one row
// band's own height (Y) — recon-measured from frame 03
// (docs/superpowers/specs/evidence/m4-recon-2026-08-12/03-r3-expanded-after-tap.png),
// same caveat as groupHeaderRegion above: real-pixel accuracy is unverified
// pending a device session, and no unit test depends on it (ocr.FakeEngine
// ignores the pixels it is handed, per its own doc comment).
const (
	nameXFrac0, nameXFrac1     = 0.19, 0.67
	powerXFrac0, powerXFrac1   = 0.19, 0.47
	levelXFrac0, levelXFrac1   = 0.48, 0.67
	statusXFrac0, statusXFrac1 = 0.69, 0.97

	topRowYFrac0, topRowYFrac1       = 0.0, 0.50
	bottomRowYFrac0, bottomRowYFrac1 = 0.50, 1.0
	statusYFrac0, statusYFrac1       = 0.0, 0.45
)

var (
	groupHeaderSpec = ocr.Spec{MinConf: 0.5}
	nameSpec        = ocr.Spec{MinConf: 0.4}
	powerSpec       = ocr.Spec{Charset: "0123456789.KMB", MinConf: 0.6}
	levelSpec       = ocr.Spec{Charset: "Lv.0123456789", MinConf: 0.6}
	lastActiveSpec  = ocr.Spec{Charset: "0123456789hmdagoOnline ", MinConf: 0.6}
)

// groupHeaderRe pulls the rank badge and the "online/total" pair out of a
// sticky header read like "R3 Footloose 8/64". The group's display name
// (here "Footloose") is user-editable and not captured — only the rank
// badge, which is stable, becomes GroupKey.
var groupHeaderRe = regexp.MustCompile(`^(R\d+)\s+.+\s(\d+)/(\d+)$`)

// parseGroupHeader extracts the rank badge and stated total from a sticky
// header's raw OCR text. An unparseable header means the frame's rows cannot
// be attributed to any group, so it must not be guessed at.
func parseGroupHeader(raw string) (groupKey string, total int, err error) {
	m := groupHeaderRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", 0, fmt.Errorf("ingest: group header %q: %w", raw, ErrUnparseable)
	}
	total, convErr := strconv.Atoi(m[3])
	if convErr != nil {
		return "", 0, fmt.Errorf("ingest: group header %q: %w", raw, ErrUnparseable)
	}
	return m[1], total, nil
}

// RosterRow is one parsed member-list row.
type RosterRow struct {
	Name            string
	NameConf        float64
	Power           int64
	Level           int
	LastActiveHours float64
	ScreenshotID    int64
	Band            RowBand
	GroupKey        string
}

// GroupTally is one rank group's row-count reconciliation: how many members
// the group's own sticky header claims against how many rows ingest actually
// collected there, via geometric dedupe. It is a count of rows, not of
// members successfully matched or created — a row that failed to parse or
// was queued for review still counts here, because reconciliation exists to
// catch rows that were never photographed at all, and an OCR failure further
// downstream must not be confused with that.
type GroupTally struct {
	Expected, Parsed int
}

// RosterResult summarizes one IngestRoster run.
type RosterResult struct {
	Matched, Created, Queued int
	PerGroup                 map[string]GroupTally
	Status                   string
}

// rosterRun carries the state one IngestRoster call accumulates across
// frames: which alliance, which members are known so far (grown in place as
// rows create new ones), and each group's expected total and dedupe cursor.
type rosterRun struct {
	captureID  int64
	allianceID int64
	members    []roster.Member
	groups     map[string]*groupTracker
	observedAt time.Time
	periodKey  string
	res        RosterResult
}

// groupTracker is one rank group's running state: its header-stated total,
// how many rows have resolved to a member (matched or created — this is what
// "group full" is checked against, per the recon's structural guard), and
// the geometric dedupe cursor.
type groupTracker struct {
	expected         int
	matchedOrCreated int
	contentY         int
	lastRowY         int // content-Y of the last collected row; -1 = none yet
}

// IngestRoster turns one roster capture's frames into members and facts.
//
// Rank is not supplied by roster_capture — capture_frames.group_key arrives
// empty, deliberately (see the M4 task amendments): group names are
// user-edited and the group set varies, so the capture task cannot assert
// which group it is looking at. Every frame carries its own evidence
// instead, in its sticky group header, which this function OCRs on every
// frame rather than trusting a label asserted elsewhere.
func (i *Ingester) IngestRoster(ctx context.Context, captureID int64) (RosterResult, error) {
	frames, err := i.store.CaptureFrames(ctx, captureID)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: loading frames for capture %d: %w", captureID, err)
	}

	allianceID, err := i.store.CurrentAllianceID(ctx)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: resolving current alliance: %w", err)
	}
	dbMembers, err := i.store.ListMembers(ctx, allianceID)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: listing members for alliance %d: %w", allianceID, err)
	}
	aliases, err := i.store.MemberAliases(ctx, allianceID)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: listing aliases for alliance %d: %w", allianceID, err)
	}

	observedAt := time.Now().UTC()
	run := &rosterRun{
		captureID:  captureID,
		allianceID: allianceID,
		members:    toRosterMembers(dbMembers, aliases),
		groups:     map[string]*groupTracker{},
		observedAt: observedAt,
		periodKey:  observedAt.Format("2006-01-02"),
		res:        RosterResult{PerGroup: map[string]GroupTally{}},
	}

	var prevGroupKey string
	havePrev := false
	var totalParsed int

	for _, frame := range frames {
		img, err := i.loadFrame(ctx, frame.ScreenshotID)
		if err != nil {
			return RosterResult{}, fmt.Errorf("ingest: loading screenshot %d: %w", frame.ScreenshotID, err)
		}

		headerRes, err := i.readField(ctx, img, groupHeaderRegion, groupHeaderSpec)
		if err != nil {
			return RosterResult{}, fmt.Errorf("ingest: reading group header on screenshot %d: %w", frame.ScreenshotID, err)
		}
		groupKey, headerTotal, herr := parseGroupHeader(headerRes.Text)
		if herr != nil {
			hy0 := int(groupHeaderRegion.Y1 * float64(img.Bounds().Dy()))
			hy1 := int(groupHeaderRegion.Y2 * float64(img.Bounds().Dy()))
			if err := run.queueReview(ctx, i, frame.ScreenshotID, RowBand{Y0: hy0, Y1: hy1}, headerRes.Text, nil, "unparseable_group_header"); err != nil {
				return RosterResult{}, err
			}
			continue
		}

		gt, exists := run.groups[groupKey]
		if !exists {
			gt = &groupTracker{expected: headerTotal, lastRowY: -1}
			run.groups[groupKey] = gt
			run.res.PerGroup[groupKey] = GroupTally{Expected: headerTotal}
		}

		newGroup := !havePrev || groupKey != prevGroupKey
		if newGroup {
			gt.contentY = 0
		} else {
			gt.contentY += frame.OffsetPx
		}
		prevGroupKey, havePrev = groupKey, true

		bands, err := SegmentRows(img, memberListRegion, memberRowPitch)
		if err != nil {
			return RosterResult{}, fmt.Errorf("ingest: segmenting screenshot %d: %w", frame.ScreenshotID, err)
		}
		if !newGroup && len(bands) > 0 {
			// The sticky header is now pinned over whatever content sits at
			// the top of the region, so the first band is a row cut in half.
			// Discard it rather than parse a partial row.
			bands = bands[1:]
		}

		regionTop := int(memberListRegion.Y1 * float64(img.Bounds().Dy()))
		for _, band := range bands {
			rowY := gt.contentY + (band.Y0 - regionTop)
			if gt.lastRowY >= 0 && rowY <= gt.lastRowY+memberRowPitch/2 {
				continue // geometric duplicate; OCR never runs on it
			}
			gt.lastRowY = rowY

			totalParsed++
			tally := run.res.PerGroup[groupKey]
			tally.Parsed++
			run.res.PerGroup[groupKey] = tally

			if err := run.processRow(ctx, i, img, band, frame.ScreenshotID, groupKey); err != nil {
				return RosterResult{}, err
			}
		}
	}

	status := "complete"
	for _, t := range run.res.PerGroup {
		if t.Parsed != t.Expected {
			status = "partial"
			break
		}
	}
	run.res.Status = status

	if err := i.store.FinishCapture(ctx, captureID, status, totalParsed, ""); err != nil {
		return RosterResult{}, fmt.Errorf("ingest: finishing capture %d: %w", captureID, err)
	}
	return run.res, nil
}

// processRow crops and reads one row's four fields, then routes it to a
// match, a creation, or the review queue.
//
// All four fields are read and the three numeric ones parsed before any
// matching happens: an unparseable field sends the whole row to review, not
// just that field, because a row this pipeline cannot fully read is a row it
// must not partially guess (design doc's ordering, not a per-field split).
func (run *rosterRun) processRow(ctx context.Context, i *Ingester, img image.Image, band RowBand, screenshotID int64, groupKey string) error {
	nameRes, err := i.readField(ctx, img, fieldRect(band, img, nameXFrac0, nameXFrac1, topRowYFrac0, topRowYFrac1), nameSpec)
	if err != nil {
		return err
	}
	powerRes, err := i.readField(ctx, img, fieldRect(band, img, powerXFrac0, powerXFrac1, bottomRowYFrac0, bottomRowYFrac1), powerSpec)
	if err != nil {
		return err
	}
	levelRes, err := i.readField(ctx, img, fieldRect(band, img, levelXFrac0, levelXFrac1, bottomRowYFrac0, bottomRowYFrac1), levelSpec)
	if err != nil {
		return err
	}
	lastRes, err := i.readField(ctx, img, fieldRect(band, img, statusXFrac0, statusXFrac1, statusYFrac0, statusYFrac1), lastActiveSpec)
	if err != nil {
		return err
	}

	row := RosterRow{
		Name: nameRes.Text, NameConf: nameRes.Confidence,
		ScreenshotID: screenshotID, Band: band, GroupKey: groupKey,
	}

	power, perr := ParsePower(powerRes.Text)
	level, lerr := ParseLevel(levelRes.Text)
	lastActive, aerr := ParseLastActiveHours(lastRes.Text)

	switch {
	case perr != nil:
		return run.queueReview(ctx, i, screenshotID, band, powerRes.Text, nil, "unparseable_power")
	case lerr != nil:
		return run.queueReview(ctx, i, screenshotID, band, levelRes.Text, nil, "unparseable_level")
	case aerr != nil:
		return run.queueReview(ctx, i, screenshotID, band, lastRes.Text, nil, "unparseable_last_active")
	}
	row.Power, row.Level, row.LastActiveHours = power, level, lastActive

	candidates := roster.Rank(row.Name, run.members)
	switch {
	case len(candidates) > 0 && candidates[0].Score >= roster.AutoAccept:
		gt := run.groups[groupKey]
		gt.matchedOrCreated++
		run.res.Matched++
		return run.writeFacts(ctx, i, row, candidates[0].MemberID, candidates[0].Score, powerRes.Confidence, levelRes.Confidence, lastRes.Confidence)

	case len(candidates) > 0 && candidates[0].Score >= roster.ReviewFloor:
		return run.queueReview(ctx, i, screenshotID, band, row.Name, candidates, "ambiguous_name_match")

	default:
		gt := run.groups[groupKey]
		if gt.matchedOrCreated < gt.expected {
			memberID, err := i.store.CreateMember(ctx, db.Member{
				AllianceID:     run.allianceID,
				Name:           row.Name,
				NameNormalized: roster.Normalize(row.Name),
				Rank:           groupKey,
			})
			if err != nil {
				return fmt.Errorf("ingest: creating member %q: %w", row.Name, err)
			}
			run.members = append(run.members, roster.Member{ID: memberID, Name: row.Name})
			gt.matchedOrCreated++
			run.res.Created++
			// A newly created member has no candidate to score against, so
			// its match component is 1.0: the row is definitionally this
			// member. Only the field's own OCR confidence gates the fact
			// from here.
			return run.writeFacts(ctx, i, row, memberID, 100, powerRes.Confidence, levelRes.Confidence, lastRes.Confidence)
		}
		// The group's own header says it is already full. A 12th "new
		// member" against an 11-member group is an OCR artifact, not a
		// person — the recon's structural guard, not a tuned threshold.
		return run.queueReview(ctx, i, screenshotID, band, row.Name, candidates, "no_confident_match_group_full")
	}
}

// writeFacts inserts one fact per numeric field, each confidence the minimum
// of the name match (normalized to [0,1]) and that field's own OCR
// confidence — the same member matched confidently can still carry a
// low-confidence power reading, and the two must not be conflated (design
// doc §4, "Confidence": "a power read at 0.6 is not a 0.95 fact"). The 0.80
// gate that number is judged against downstream is not enforced here as a
// write-time branch: a row only reaches this method after clearing the
// name-match and field-parse gates in processRow, and at that point its true
// blended confidence is stored, whatever it is, for a later query to filter
// on — "not a 0.95 fact" is not the same claim as "not written at all".
func (run *rosterRun) writeFacts(ctx context.Context, i *Ingester, row RosterRow, memberID int64, matchScore int, powerConf, levelConf, lastConf float64) error {
	matchNorm := float64(matchScore) / 100.0
	facts := [3]struct {
		metric string
		value  float64
		conf   float64
	}{
		{"power", float64(row.Power), min(matchNorm, powerConf)},
		{"level", float64(row.Level), min(matchNorm, levelConf)},
		{"last_active_hours", row.LastActiveHours, min(matchNorm, lastConf)},
	}
	for _, f := range facts {
		if _, err := i.store.InsertFact(ctx, db.Fact{
			MemberID: memberID, Metric: f.metric, Value: f.value,
			ObservedAt: run.observedAt, PeriodKey: run.periodKey,
			Source: "ocr:alliance_members", ScreenshotID: row.ScreenshotID,
			Confidence: f.conf,
		}); err != nil {
			return fmt.Errorf("ingest: writing %s fact for member %d: %w", f.metric, memberID, err)
		}
	}
	return nil
}

// queueReview records a row (or a whole-frame header) that could not be
// confidently resolved, and counts it. Never called for a row that was
// matched or created.
func (run *rosterRun) queueReview(ctx context.Context, i *Ingester, screenshotID int64, band RowBand, rawText string, candidates []roster.Candidate, reason string) error {
	if _, err := i.store.QueueReview(ctx, db.ReviewItem{
		CaptureID: run.captureID, ScreenshotID: screenshotID,
		RowY0: band.Y0, RowY1: band.Y1,
		RawText: rawText, Candidates: candidates, Reason: reason,
	}); err != nil {
		return fmt.Errorf("ingest: queueing review (%s): %w", reason, err)
	}
	run.res.Queued++
	return nil
}

// toRosterMembers folds ListMembers and MemberAliases into roster.Member,
// which carries aliases inline for roster.Rank. db.Member and roster.Member
// are deliberately distinct types (the matcher's view, and the database's).
func toRosterMembers(dbMembers []db.Member, aliases map[int64][]string) []roster.Member {
	out := make([]roster.Member, len(dbMembers))
	for idx, m := range dbMembers {
		out[idx] = roster.Member{ID: m.ID, Name: m.Name, Aliases: aliases[m.ID]}
	}
	return out
}

// fieldRect places one field's crop within a row band, as a rect normalized
// to the full frame: xFrac0/1 are fractions of the frame width (columns are
// frame-relative, not row-relative), yFrac0/1 are fractions of the row
// band's own height (rows vary slightly in detected height, so the field
// must scale with the band it was found in).
func fieldRect(band RowBand, img image.Image, xFrac0, xFrac1, yFrac0, yFrac1 float64) transport.Rect {
	h := float64(img.Bounds().Dy())
	rowH := float64(band.Height())
	return transport.Rect{
		X1: xFrac0, X2: xFrac1,
		Y1: (float64(band.Y0) + yFrac0*rowH) / h,
		Y2: (float64(band.Y0) + yFrac1*rowH) / h,
	}
}

// readField preprocesses one region and reads it with the engine.
func (i *Ingester) readField(ctx context.Context, img image.Image, rect transport.Rect, spec ocr.Spec) (ocr.Result, error) {
	pre := vision.Preprocess(img, vision.Options{Region: rect})
	res, err := i.engine.Read(ctx, pre, spec)
	if err != nil {
		return ocr.Result{}, fmt.Errorf("ingest: ocr read: %w", err)
	}
	return res, nil
}

// loadFrame resolves a screenshot to its blob and decodes it. Screenshots
// are always PNG (adb exec-out screencap -p; see CLAUDE.md gotchas).
func (i *Ingester) loadFrame(ctx context.Context, screenshotID int64) (image.Image, error) {
	key, err := i.store.ScreenshotObjectKey(ctx, screenshotID)
	if err != nil {
		return nil, fmt.Errorf("ingest: resolving object key for screenshot %d: %w", screenshotID, err)
	}
	rc, err := i.blobs.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("ingest: fetching blob %s for screenshot %d: %w", key, screenshotID, err)
	}
	defer rc.Close()
	img, err := png.Decode(rc)
	if err != nil {
		return nil, fmt.Errorf("ingest: decoding screenshot %d: %w", screenshotID, err)
	}
	return img, nil
}
