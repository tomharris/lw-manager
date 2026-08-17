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
