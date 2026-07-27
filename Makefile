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
