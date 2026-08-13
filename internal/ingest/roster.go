package ingest

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"log/slog"
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

// The MinConf values below are advisory, not enforced: nothing in this file
// calls ocr.Result.Accepted(spec), so a read below its field's MinConf is
// neither rejected nor rerouted on that basis alone. The floor actually
// applied is the flat factConfidenceGate (0.80) in writeFacts, against each
// field's blended (name-match × OCR) confidence — which exceeds every value
// declared here, so today this causes no wrong fact to be written. The
// per-field numbers are kept as documentation of each field's OCR
// difficulty (a free-text name at 0.4 versus a constrained-charset number at
// 0.6) and as the Charset each field is actually read with; they are not a
// second gate.
var (
	groupHeaderSpec = ocr.Spec{MinConf: 0.5}
	nameSpec        = ocr.Spec{MinConf: 0.4}
	powerSpec       = ocr.Spec{Charset: "0123456789.KMB", MinConf: 0.6}
	levelSpec       = ocr.Spec{Charset: "Lv.0123456789", MinConf: 0.6}
	lastActiveSpec  = ocr.Spec{Charset: "0123456789hmdagoOnline ", MinConf: 0.6}
)

// allianceMemberCountRegion is the band on the alliance screen (not
// alliance_members) carrying "Members: 96/100" — recon frame 01
// (docs/superpowers/specs/evidence/m4-recon-2026-08-12/01-alliance-members-96-of-100.png).
// Unverified pending a device session, same caveat as groupHeaderRegion and
// the field fractions below: no unit test depends on its accuracy, since
// ocr.FakeEngine ignores the pixels it is handed.
var allianceMemberCountRegion = transport.Rect{X1: 0.03, Y1: 0.19, X2: 0.60, Y2: 0.27}

// See roster.go's Spec-value doc comment above groupHeaderSpec: advisory,
// not enforced — factConfidenceGate is not applied to this read at all,
// since it never becomes a Fact; a failed read just degrades the
// alliance-total check (see readAllianceMemberCount).
var allianceMemberCountSpec = ocr.Spec{MinConf: 0.5}

// allianceMemberCountRe pulls the alliance's current member count out of the
// alliance screen's "Members: 96/100" line. Only the first number is the
// roster's reconciliation ground truth — the second is the alliance's
// capacity, not a headcount (recon: "R5 1 + R4 9 + R3 64 + R2 11 + R1 11 =
// 96 = Members: 96/100").
var allianceMemberCountRe = regexp.MustCompile(`Members:\s*(\d+)\s*/\s*\d+`)

// parseAllianceMemberCount extracts the alliance's current member count from
// the alliance frame's raw OCR text. An unparseable read means the
// alliance-total check cannot run this pass, not that the alliance has zero
// members — callers must treat the returned error as "unavailable", never
// substitute a zero (see readAllianceMemberCount).
func parseAllianceMemberCount(raw string) (int, error) {
	m := allianceMemberCountRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, fmt.Errorf("ingest: alliance member count %q: %w", raw, ErrUnparseable)
	}
	count, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("ingest: alliance member count %q: %w", raw, ErrUnparseable)
	}
	return count, nil
}

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

	// AllianceMemberCount is the "96" read from the alliance frame's
	// "Members: 96/100" line — the same number written to
	// alliances.member_count. Zero when AllianceTotalChecked is false.
	AllianceMemberCount int
	// AllianceTotalChecked reports whether the sum of every group's parsed
	// row count was reconciled against AllianceMemberCount this run. False
	// means the alliance frame was missing from the capture, or present but
	// unreadable — the alliance-total check is simply unavailable in that
	// case, and Status reflects per-group reconciliation alone, exactly as
	// it did before this check existed. Losing a whole roster capture to one
	// frame's bad OCR would cost more than the check being unavailable.
	AllianceTotalChecked bool
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
// empty on every member-list frame, deliberately (see the M4 task
// amendments): group names are user-edited and the group set varies, so the
// capture task cannot assert which group it is looking at. Every frame
// carries its own evidence instead, in its sticky group header, which this
// function OCRs on every frame rather than trusting a label asserted
// elsewhere.
//
// One frame is the deliberate exception: the alliance screen's summary
// frame, tagged vision.AllianceSummaryGroupKey by roster_capture, is not a
// list frame at all. It is pulled out before the member-list loop below,
// read for its own "Members: 96/100" total (readAllianceMemberCount), and
// never handed to SegmentRows — running row segmentation over it would
// produce garbage bands.
//
// periodKey is supplied by the caller rather than computed here, matching
// IngestVS's shape — both routes take it as an explicit argument so the
// caller (cmd/control's `ingest` command) derives it once, from the
// capture's own started_at, and neither route can silently fall back to
// wall-clock now. observedAt (the facts' own timestamp) is likewise pinned
// to the capture's started_at rather than time.Now(): a parser fix replayed
// over a capture from months ago must write the same facts it would have
// written the day the screenshot was taken, and stamping "now" on either
// value would defeat that.
func (i *Ingester) IngestRoster(ctx context.Context, captureID int64, periodKey string) (RosterResult, error) {
	capture, err := i.store.Capture(ctx, captureID)
	if err != nil {
		return RosterResult{}, fmt.Errorf("ingest: loading capture %d: %w", captureID, err)
	}
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

	run := &rosterRun{
		captureID:  captureID,
		allianceID: allianceID,
		members:    toRosterMembers(dbMembers, aliases),
		groups:     map[string]*groupTracker{},
		observedAt: capture.StartedAt,
		periodKey:  periodKey,
		res:        RosterResult{PerGroup: map[string]GroupTally{}},
	}

	// The alliance frame (vision.AllianceSummaryGroupKey) is not a list
	// screen and must never reach row segmentation — pull it out before the
	// main loop rather than let the loop special-case it inline, so the
	// newGroup/prevGroupKey bookkeeping below, which assumes every frame it
	// sees is a scrolled member-list frame, never has to know this one
	// exists. It is expected to be missing from captures recorded before
	// this check existed, and IngestRoster must still ingest those exactly
	// as before.
	var allianceFrame *db.CaptureFrame
	listFrames := make([]db.CaptureFrame, 0, len(frames))
	for idx := range frames {
		if frames[idx].GroupKey == vision.AllianceSummaryGroupKey {
			f := frames[idx]
			allianceFrame = &f
			continue
		}
		listFrames = append(listFrames, frames[idx])
	}

	if allianceFrame != nil {
		if count, ok := run.readAllianceMemberCount(ctx, i, *allianceFrame); ok {
			run.res.AllianceMemberCount = count
			run.res.AllianceTotalChecked = true
		}
	}

	var prevGroupKey string
	havePrev := false
	var totalParsed int

	for _, frame := range listFrames {
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
			if err := run.queueReview(ctx, i, frame.ScreenshotID, RowBand{Y0: hy0, Y1: hy1}, headerRes.Text, nil, "unparseable_group_header", 0); err != nil {
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
			// The design doc also specifies an identity-based cross-check
			// alongside this geometric dedupe, flagging a disagreement
			// between the two counts. It is deliberately not implemented
			// here. That check exists to catch a member's row appearing at
			// two different screen positions at once — which geometric
			// dedupe structurally cannot detect, since it only compares a
			// row's position to the previous one — and the only place recon
			// observed that phenomenon is the VS ranking's pinned self row
			// (docs/superpowers/specs/2026-08-12-m4-recon-findings.md §2:
			// "the self row is pinned... and also appears in its natural
			// position"). Recon's roster notes (§1) record no such
			// duplicate: the logged-in account's row is unremarkable except
			// for lacking a Manage button. A pinned row is a property of the
			// VS ranking screen, not of alliance_members, so the identity
			// cross-check belongs to the VS ingest path, not here.
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

	// The alliance-total check is additional to per-group reconciliation
	// above, not a replacement for it: it catches what per-group checks
	// structurally cannot, since it sums what was actually parsed rather
	// than depending on every group having been seen at all. A group whose
	// frames never made it into this capture leaves no PerGroup entry to
	// fall short — the loop above would call that "complete" — but the sum
	// of every group that *was* seen still falls short of the alliance's own
	// count, and only this check catches it.
	if run.res.AllianceTotalChecked && totalParsed != run.res.AllianceMemberCount {
		status = "partial"
		slog.WarnContext(ctx, "ingest: roster alliance-total reconciliation found a mismatch",
			"capture_id", captureID, "parsed_total", totalParsed, "alliance_member_count", run.res.AllianceMemberCount)
	}
	run.res.Status = status

	// Recorded whenever the read succeeded, independent of whether it
	// reconciled — the alliance screen's own count is worth keeping even
	// when the parsed rows do not add up to it, per the M4 task-11b brief
	// ("populate alliances.member_count from the same read").
	if run.res.AllianceTotalChecked {
		if err := i.store.SetAllianceMemberCount(ctx, allianceID, run.res.AllianceMemberCount); err != nil {
			return RosterResult{}, fmt.Errorf("ingest: recording alliance %d member count: %w", allianceID, err)
		}
	}

	if err := i.store.FinishCapture(ctx, captureID, status, totalParsed, ""); err != nil {
		return RosterResult{}, fmt.Errorf("ingest: finishing capture %d: %w", captureID, err)
	}
	return run.res, nil
}

// readAllianceMemberCount OCRs and parses the alliance frame's
// "Members: 96/100" line. Any failure — the blob missing, the OCR engine
// erroring, or the text not matching the expected shape — degrades to
// "unavailable" (ok == false) rather than propagating: per the M4 task-11b
// brief, losing an entire otherwise-good roster capture to one frame's bad
// OCR would cost more than the alliance-total check simply not running this
// pass. IngestRoster falls back to per-group reconciliation alone in that
// case, exactly as it did before this check existed.
func (run *rosterRun) readAllianceMemberCount(ctx context.Context, i *Ingester, frame db.CaptureFrame) (int, bool) {
	img, err := i.loadFrame(ctx, frame.ScreenshotID)
	if err != nil {
		slog.WarnContext(ctx, "ingest: could not load the alliance frame; alliance-total reconciliation unavailable this run",
			"capture_id", run.captureID, "screenshot_id", frame.ScreenshotID, "error", err)
		return 0, false
	}
	res, err := i.readField(ctx, img, allianceMemberCountRegion, allianceMemberCountSpec)
	if err != nil {
		slog.WarnContext(ctx, "ingest: could not OCR the alliance frame's member count; alliance-total reconciliation unavailable this run",
			"capture_id", run.captureID, "screenshot_id", frame.ScreenshotID, "error", err)
		return 0, false
	}
	count, err := parseAllianceMemberCount(res.Text)
	if err != nil {
		slog.WarnContext(ctx, "ingest: could not parse the alliance frame's member count; alliance-total reconciliation unavailable this run",
			"capture_id", run.captureID, "screenshot_id", frame.ScreenshotID, "raw_text", res.Text)
		return 0, false
	}
	return count, true
}

// processRow crops and reads one row's four fields, then routes it to a
// match, a creation, or the review queue.
//
// Name resolution and numeric-field parsing are independent: a row's three
// numeric fields are parsed but not gated on one another, and matching the
// name proceeds regardless of what happened to power/level/last-active.
// Facts are per-metric (the M4 amendment on this task), so an unparseable
// power field must not cost a row its perfectly good level and
// last_active_hours — only that one field is queued for review, and
// writeFacts is what makes that split per-field rather than per-row.
//
// The one thing numeric parsing cannot survive is a name that fails to
// resolve to a member at all: without a memberID no fact has anywhere to
// attach, so an ambiguous or unmatched-and-group-full name still sends the
// whole row to review, same as before.
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

	power, perr := ParsePower(powerRes.Text)
	level, lerr := ParseLevel(levelRes.Text)
	lastActive, aerr := ParseLastActiveHours(lastRes.Text)

	row := RosterRow{
		Name: nameRes.Text, NameConf: nameRes.Confidence,
		Power: power, Level: level, LastActiveHours: lastActive,
		ScreenshotID: screenshotID, Band: band, GroupKey: groupKey,
	}

	candidates := roster.Rank(row.Name, run.members)
	switch {
	case len(candidates) > 0 && candidates[0].Score >= roster.AutoAccept:
		gt := run.groups[groupKey]
		gt.matchedOrCreated++
		run.res.Matched++
		matchNorm := float64(candidates[0].Score) / 100.0
		return run.writeFacts(ctx, i, screenshotID, band, candidates[0].MemberID, matchNorm, fieldReads(row, powerRes, levelRes, lastRes, perr, lerr, aerr))

	case len(candidates) > 0 && candidates[0].Score >= roster.ReviewFloor:
		return run.queueReview(ctx, i, screenshotID, band, row.Name, candidates, "ambiguous_name_match", 0)

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
			// member. Only each field's own OCR confidence gates its fact
			// from here.
			return run.writeFacts(ctx, i, screenshotID, band, memberID, 1.0, fieldReads(row, powerRes, levelRes, lastRes, perr, lerr, aerr))
		}
		// The group's own header says it is already full. A 12th "new
		// member" against an 11-member group is an OCR artifact, not a
		// person — the recon's structural guard, not a tuned threshold.
		return run.queueReview(ctx, i, screenshotID, band, row.Name, candidates, "no_confident_match_group_full", 0)
	}
}

// fieldRead is one numeric field's OCR read, parse outcome, and the label
// used to name it in a review row's reason ("power", "level",
// "last_active") — kept distinct from Fact.Metric ("last_active_hours")
// so a reviewer sees the same word the field crop is named after.
type fieldRead struct {
	metric, label string
	value, conf   float64
	raw           string
	err           error
}

// fieldReads assembles the three numeric fields' reads for writeFacts, in a
// fixed order so a partially-failed row always reports power, then level,
// then last-active, regardless of which one failed.
func fieldReads(row RosterRow, powerRes, levelRes, lastRes ocr.Result, perr, lerr, aerr error) [3]fieldRead {
	return [3]fieldRead{
		{metric: "power", label: "power", value: float64(row.Power), conf: powerRes.Confidence, raw: powerRes.Text, err: perr},
		{metric: "level", label: "level", value: float64(row.Level), conf: levelRes.Confidence, raw: levelRes.Text, err: lerr},
		{metric: "last_active_hours", label: "last_active", value: row.LastActiveHours, conf: lastRes.Confidence, raw: lastRes.Text, err: aerr},
	}
}

// factConfidenceGate is the floor CLAUDE.md invariant #5 sets: "low-confidence
// reads go to the review queue, never to a leaderboard." Facts are exactly
// what a leaderboard reads, so the gate is enforced here, at write time, not
// as a filter a later query is trusted to remember — the first query that
// forgets would otherwise put an unverifiable number in front of a real
// alliance.
const factConfidenceGate = 0.80

// writeFacts resolves each of a row's three numeric fields independently:
// one that failed to parse, or one whose own OCR text a matched member's row
// carries, is queued for review rather than dropped — but that verdict is
// per field, not per row, so a garbled power reading does not cost the row
// its perfectly good level and last_active_hours.
//
// A field's confidence is the minimum of the name match (normalized to
// [0,1]) and that field's own OCR confidence — the same member matched
// confidently can still carry a low-confidence power reading, and the two
// must not be conflated (design doc §4, "Confidence": "a power read at 0.6
// is not a 0.95 fact"). Below factConfidenceGate that confidence is not "a
// 0.95 fact" — CLAUDE.md's invariant #5 makes it not a fact at all, so the
// field is queued for review instead, carrying its own blended confidence so
// a human can see how bad the read was.
func (run *rosterRun) writeFacts(ctx context.Context, i *Ingester, screenshotID int64, band RowBand, memberID int64, matchNorm float64, fields [3]fieldRead) error {
	for _, f := range fields {
		if f.err != nil {
			if err := run.queueReview(ctx, i, screenshotID, band, f.raw, nil, "unparseable_"+f.label, 0); err != nil {
				return err
			}
			continue
		}
		conf := min(matchNorm, f.conf)
		if conf < factConfidenceGate {
			if err := run.queueReview(ctx, i, screenshotID, band, f.raw, nil, "low_confidence_"+f.label, conf); err != nil {
				return err
			}
			continue
		}
		if _, err := i.store.InsertFact(ctx, db.Fact{
			MemberID: memberID, Metric: f.metric, Value: f.value,
			ObservedAt: run.observedAt, PeriodKey: run.periodKey,
			Source: "ocr:alliance_members", ScreenshotID: screenshotID,
			Confidence: conf,
		}); err != nil {
			return fmt.Errorf("ingest: writing %s fact for member %d: %w", f.metric, memberID, err)
		}
	}
	return nil
}

// queueReview records a row, a whole-frame header, or a single field that
// could not be confidently resolved (or cleared to write), and counts it.
// confidence is 0 when the reason carries no meaningful blended score (an
// unparseable field, an ambiguous name, a full group) — QueueReview stores
// that as SQL NULL, not as a claimed zero-confidence read.
func (run *rosterRun) queueReview(ctx context.Context, i *Ingester, screenshotID int64, band RowBand, rawText string, candidates []roster.Candidate, reason string, confidence float64) error {
	if _, err := i.store.QueueReview(ctx, db.ReviewItem{
		CaptureID: run.captureID, ScreenshotID: screenshotID,
		RowY0: band.Y0, RowY1: band.Y1,
		RawText: rawText, Candidates: candidates, Reason: reason,
		Confidence: confidence,
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
