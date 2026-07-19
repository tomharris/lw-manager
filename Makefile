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
