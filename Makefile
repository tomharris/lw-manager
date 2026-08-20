export CGO_ENABLED := 0

GO ?= go
BIN := bin

.PHONY: all
all: build

.PHONY: build
build:
	$(GO) build -o $(BIN)/agent ./cmd/agent
	$(GO) build -o $(BIN)/control ./cmd/control

# Unit tests. Must pass with no emulator, no adb, and no Docker running.
.PHONY: test
test:
	$(GO) test ./...

# Tests that need the compose stack (postgres, minio).
.PHONY: test-integration
test-integration:
	$(GO) test -tags=integration ./...

# Tests that need a real device on adb. Separate from `integration` because
# the infrastructure is different: an emulator or handset, not Docker. They
# skip rather than fail when no device is attached.
.PHONY: test-device
test-device:
	$(GO) test -tags=device -count=1 ./internal/transport/...

# The M1 phase gate: recognizer accuracy against the real corpus. Device-free
# but slow, so it is tagged out of `make test`. Skips when the corpus has not
# been pulled.
#
# -timeout is explicit because the default is 10m and this target is designed
# to get slower: cost is frames x anchors, both of which grow every time the
# corpus or the manifest is extended, and the matcher is a hand-rolled NCC
# under CGO_ENABLED=0. 356 frames against 41 anchors already takes ~14m. The
# failure mode is worth recognizing on sight — a panic with a goroutine dump
# at exactly 600s is the timeout, not the gate, and says nothing about
# accuracy. Raise this rather than trimming the corpus to fit it.
.PHONY: gate
gate:
	$(GO) test -tags=corpus -count=1 -v -timeout 60m ./internal/vision/...

# The M4 phase gate: ingest reproduces a hand-checked VS ranking capture.
#
# Device-free, but not dependency-free — unlike `gate` it needs three things at
# once: the compose stack (it prepares lw_manager_test through internal/dbtest,
# never the dev database), the blob store the capture was written to, and the
# tesseract binary. It skips, naming what is missing, when the hand-checked
# fixture has not been transcribed yet or its frames are not in the configured
# blob store — see fixtures/m4gate/README.md.
#
# LW_BLOB_FS_ROOT is defaulted to an absolute path here because the fs
# backend's own default is the relative ./data/blobs, and `go test` runs each
# package binary in its own source directory — so the gate would look for the
# capture's frames under internal/ingest/data/blobs and skip, reporting a
# missing frame rather than a mislocated store. ?= so an explicitly configured
# store still wins, and it is simply ignored when the backend is s3.
.PHONY: gate-m4
gate-m4: LW_BLOB_FS_ROOT ?= $(CURDIR)/data/blobs
gate-m4:
	LW_BLOB_FS_ROOT="$(LW_BLOB_FS_ROOT)" $(GO) test -tags m4gate -count=1 -v -timeout 20m ./internal/ingest/

# The M4 roster gate: ingest reproduces a hand-transcribed roster capture.
#
# Same three dependencies as gate-m4 and for the same reasons — Postgres via
# internal/dbtest (never the dev database), the blob store holding capture 1's
# frames, and tesseract. It skips, naming what is missing, when the
# transcription is absent or a frame is not in the configured store.
#
# LW_BLOB_FS_ROOT must be ABSOLUTE, for the reason spelled out above gate-m4:
# `go test` runs each package binary in its own source directory, so the fs
# backend's relative ./data/blobs would resolve under internal/ingest and find
# nothing. ?= so an explicitly configured store still wins.
.PHONY: gate-roster
gate-roster: LW_BLOB_FS_ROOT ?= $(CURDIR)/data/blobs
gate-roster:
	LW_BLOB_FS_ROOT="$(LW_BLOB_FS_ROOT)" $(GO) test -tags m4rostergate -count=1 -v -timeout 20m ./internal/ingest/

# The M4 name probe: a measuring instrument for the name field, not a gate.
#
# It asserts nothing and always passes; its output is the point. Use it to
# decide anything about the name crop, the preprocessing options, the language
# packs or the matcher's scoring — the numbers that set today's vsNameOptions
# came from a harness like this that was NOT committed, and rebuilding it cost
# a session. Needs the blob store and tesseract; no database.
#
#	make probe-m4                                # the shipped setting
#	make probe-m4 PROBE_ARGS='-probe.detail'     # per-member, to localize
#	make probe-m4 PROBE_ARGS='-probe.sweep'      # the full options sweep
#	make probe-m4 PROBE_ARGS='-probe.langs=eng+kor+ara+chi_sim+jpn -probe.detail'
.PHONY: probe-m4
probe-m4: LW_BLOB_FS_ROOT ?= $(CURDIR)/data/blobs
probe-m4:
	LW_BLOB_FS_ROOT="$(LW_BLOB_FS_ROOT)" $(GO) test -tags m4probe -count=1 -v -timeout 60m ./internal/ingest/ -run TestM4NameProbe $(PROBE_ARGS)

# The M4 points probe: the same instrument for the *points* field.
#
# It is separate from probe-m4 rather than a flag on it because the two answer
# different questions and share only a fixture. The gate's row count moves when
# the name fails and when the points fail, and those need opposite fixes: of the
# gate's 21 failures, 8 are rows whose name reads perfectly.
#
# Its headline number is roster-free — parsed values scored against the known
# points, with no name matching — so the points field can be measured
# independently of the name field's accuracy. -points.detail adds the name, and
# is the only mode that can attribute an empty or unparseable read to a row.
#
#	make probe-points                              # the shipped setting
#	make probe-points PROBE_ARGS='-points.detail'  # per-row, to localize
#	make probe-points PROBE_ARGS='-points.sweep'   # the full options sweep
#	make probe-points PROBE_ARGS='-points.charset=0123456789,'  # re-measure the no-charset decision
.PHONY: probe-points
probe-points: LW_BLOB_FS_ROOT ?= $(CURDIR)/data/blobs
probe-points:
	LW_BLOB_FS_ROOT="$(LW_BLOB_FS_ROOT)" $(GO) test -tags m4probe -count=1 -v -timeout 60m ./internal/ingest/ -run TestM4PointsProbe $(PROBE_ARGS)

# The M4 assignment probe: does closed-set matching beat per-row thresholding,
# and at what false-attribution cost?
#
# Not a gate. It asserts nothing and its output is the point. Reach for it
# before changing roster.ResidualFloor, roster.ResidualMargin or
# residualMatchConfidence -- and after, because "re-measure, do not re-reason"
# applies to all three.
#
# Two of its modes exist to keep its own numbers honest, and both should be run
# before believing a headline:
#
#   -probe.assignshuffle   rotates the truth labels by one rank, so every
#                          assignment is wrong by construction. It must report
#                          ~0 correct. A clean run here proved nothing until
#                          this fired, because forcing an assignment at floor 0
#                          / margin 0 produced a PERFECT result -- which reads
#                          as a finding and is really an untested instrument.
#   -probe.assigndecoys=N  pads the member set with N members one confusable
#                          substitution from a real name. The gate's capture is
#                          square (86 rows, 86 members) and production is not
#                          (recon: 94 ranked rows, 96 alliance members), and
#                          squareness is the assignment's biggest unearned
#                          advantage.
#
#	make probe-assign
#	make probe-assign PROBE_ARGS='-probe.assigndetail'
#	make probe-assign PROBE_ARGS='-probe.assignshuffle'
#	make probe-assign PROBE_ARGS='-probe.assigndecoys=20 -probe.assignpsm=13'
.PHONY: probe-assign
probe-assign: LW_BLOB_FS_ROOT ?= $(CURDIR)/data/blobs
probe-assign:
	LW_BLOB_FS_ROOT="$(LW_BLOB_FS_ROOT)" $(GO) test -tags m4probe -count=1 -v -timeout 60m ./internal/ingest/ -run TestM4AssignProbe -probe.assign $(PROBE_ARGS)

# make probe-roster -- NOT a gate: the measuring instrument for the roster
# route's name column, and the roster's counterpart to probe-m4 / probe-points
# / probe-assign. It asserts nothing and always passes; reading its output is
# the point. Needs the blob store and tesseract, no database.
#
# It scores against fixtures/m4rostergate/expected.yaml -- 75 members, THIS
# capture, transcribed frame by frame. It used to score against the VS
# fixture's 86 ranked names, three days later and missing 11 of this roster's
# members, which is why every number it printed carried a "lower bound, never
# an accuracy" caveat. That caveat is retired: `exact` is an accuracy and
# `unmatched` is an error rate.
#
# Two columns still need their own reading. `junk-prefixed` counts reads that
# are a known name plus one leading token -- provably-correct reads with
# something the crop let in, and the column a crop change is read against
# first. `exact (below MinConf)` counts reads that are byte-identical to a
# transcribed name and still below nameSpec.MinConf, which processRow refuses
# to create a member from: an accuracy count hides those entirely.
#
#   -roster.detail      per-band reads and verdicts, to localize
#   -roster.members     per-MEMBER: each member's best band and what
#                       processRow would do with it (MATCH / CREATABLE /
#                       LOW-CONF / MISS). This is the view the gate's
#                       "member never created" question needs; a per-band
#                       count cannot answer it.
#   -roster.noretry     read at PSM 7 only, without the PSM 13 retry
#                       production ships -- what the retry is worth. The
#                       default is production's own read path.
#   -roster.x0sweep     sweep nameXFrac0 across the gutter
#   -roster.inkprofile  the column histogram the crop edges are placed from
#
# Five more instruments measure the fields the name probe never touched.
# Between them they cover every fact this route fails to write, and the first
# is the only instrument in this file aimed at something that is not an OCR
# read:
#
#   -roster.badge       matchRankBadge's per-frame verdict, with the gap
#                       distribution split by right/wrong -- a wrong verdict at
#                       a wide gap means the templates match the wrong thing, at
#                       a narrow gap means the threshold cannot separate them,
#                       and those need opposite fixes. Rank comes from NCC, not
#                       OCR, so no other mode here can see a rank defect.
#   -roster.badgeshuffle  not a sixth instrument -- rotates -roster.badge's own
#                       truth table one rank forward so the mode is wrong by
#                       construction. It must report 0 agree; run it before
#                       believing a clean badge sweep. Implies -roster.badge.
#   -roster.header      the sticky group header's raw text beside
#                       parseGroupHeader's verdict, so a refusal names its own
#                       cause. A header that will not parse drops the WHOLE
#                       frame before any row is read.
#   -roster.headerink   the header's column histogram, printed at full frame
#                       width rather than the name field's 0.10-0.45 window,
#                       because the edge under suspicion was X2=0.97. Read the
#                       numeric column, not the bars: the bar renders any count
#                       under 65 as empty, so it cannot tell 0 from 16.
#   -roster.headersweep the header crop's edges walked across that gutter, and
#                       the count-only rectangle beside it, each scored against
#                       the transcribed GROUP TOTALS rather than only on
#                       whether it parsed -- an under-count is the failure that
#                       does silent damage, so a fabrication must not outrank a
#                       refusal.
#   -roster.headeropts  24 preprocessing shapes (8 skip-flag combinations x 3
#                       upscale factors) through EACH candidate rectangle, then
#                       PSM 8/11/13 through the count-only one, then a
#                       "0123456789/" whitelist through both. Options measured
#                       through the wrong rectangle are not evidence about the
#                       right one, so moving the crop obliges this. The
#                       whitelist is measured, not endorsed: it reads R2 as
#                       nothing and manufactures a total of 1 for an 11-member
#                       group on four frames.
#   -roster.headerthresh  AdaptiveThreshold's block size and C, the two knobs
#                       the shape grid never varies: 40 settings through EACH
#                       rectangle. R2's "1/11" resists all of them -- tesseract
#                       classifies that run of vertical bars as "VN", "VL",
#                       "Wu" or "U/L" -- which is the engine, not the crop and
#                       not the contrast the mode was built to chase.
#   -roster.power       the power column's reads and ParsePower's verdict,
#                       counting refusals that are structurally one damaged
#                       separator -- the shape the review queue is full of and
#                       the number a crop change should move.
#   -roster.level       the same for the level column and ParseLevel.
#
#	make probe-roster
#	make probe-roster PROBE_ARGS='-roster.detail'
#	make probe-roster PROBE_ARGS='-roster.members'
#	make probe-roster PROBE_ARGS='-roster.x0sweep'
#	make probe-roster PROBE_ARGS='-roster.inkprofile -roster.maxframes=12'
#	make probe-roster PROBE_ARGS='-roster.badge'
#	make probe-roster PROBE_ARGS='-roster.badge -roster.badgeshuffle'
#	make probe-roster PROBE_ARGS='-roster.header'
#	make probe-roster PROBE_ARGS='-roster.headerink'
#	make probe-roster PROBE_ARGS='-roster.headersweep'
#	make probe-roster PROBE_ARGS='-roster.headeropts'
#	make probe-roster PROBE_ARGS='-roster.headerthresh'
#	make probe-roster PROBE_ARGS='-roster.power'
#	make probe-roster PROBE_ARGS='-roster.level'
.PHONY: probe-roster
probe-roster: LW_BLOB_FS_ROOT ?= $(CURDIR)/data/blobs
probe-roster:
	LW_BLOB_FS_ROOT="$(LW_BLOB_FS_ROOT)" $(GO) test -tags m4probe -count=1 -v -timeout 60m ./internal/ingest/ -run TestRosterNameProbe $(PROBE_ARGS)

.PHONY: lint
lint:
	$(GO) vet ./...
	gofmt -l -d .

# Fails loudly if anything drags in a cgo dependency.
.PHONY: verify-nocgo
verify-nocgo:
	CGO_ENABLED=0 $(GO) build ./...

.PHONY: up
up:
	docker compose up -d

.PHONY: down
down:
	docker compose down

.PHONY: migrate
migrate:
	$(GO) run ./cmd/control migrate

.PHONY: sqlc
sqlc:
	sqlc generate

.PHONY: clean
clean:
	rm -rf $(BIN)
