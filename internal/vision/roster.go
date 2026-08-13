package vision

// AllianceSummaryGroupKey marks the capture_frames row that holds the
// alliance screen's summary frame — tag, name, leader, and the
// "Members: 96/100" reconciliation ground truth — which roster_capture
// captures once per run, before the rank-group loop walks the member list
// (see internal/tasks/roster_capture.go and internal/ingest/roster.go).
//
// It lives here, rather than in either of those packages, because both need
// it and neither may import the other: internal/tasks depends on
// internal/runtime, which the device-free internal/ingest package must not
// pull in. internal/vision is the shared ground both already stand on.
//
// The value is a sentinel outside the "R\d+" shape a real rank-group
// group_key takes (see ingest's groupHeaderRe), so it can never collide with
// one. It marks the one capture_frames row that must never reach row
// segmentation — the alliance screen is not a list screen, and running
// SegmentRows over it would produce garbage bands.
const AllianceSummaryGroupKey = "_alliance_summary"
