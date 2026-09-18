# R1 build entry points. The binary has no CLI; these targets are for developers and CI.
SHELL := /bin/bash
export PATH := $(PATH):$(HOME)/go/bin:$(HOME)/.cargo/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/acx1729/ocean/internal/version.Version=$(VERSION) -X github.com/acx1729/ocean/internal/version.Commit=$(COMMIT)
LORO_LIB := rust/loro-cabi/target/release/libloro_cabi.a

.PHONY: all build loro proto lint test test-short web e2e docker dev-env dev-db dev-db-stop clean tools

all: build

## loro: build the Rust C-ABI shim over the loro crate (needed by CGO)
loro: $(LORO_LIB)
$(LORO_LIB): rust/loro-cabi/Cargo.toml rust/loro-cabi/Cargo.lock rust/loro-cabi/src/*.rs
	cd rust/loro-cabi && cargo build --release

## build: compile the single server binary into bin/kb
build: loro
	CGO_ENABLED=1 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/kb ./cmd/kb

## proto: regenerate gen/ from proto/ (requires buf, protoc-gen-go, protoc-gen-connect-go)
proto:
	buf lint
	rm -rf gen && buf generate

## tools: install code generators
tools:
	go install github.com/bufbuild/buf/cmd/buf@v1.73.0
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.0

## lint: vet and formatting checks
lint:
	test -z "$$(gofmt -l cmd internal | tee /dev/stderr)"
	go vet ./...

## dev-db: start a disposable Postgres 16 for tests and local runs (127.0.0.1:55432)
dev-db:
	scripts/dev-postgres.sh start

dev-db-stop:
	scripts/dev-postgres.sh stop

## test: the full Go suite; database tests need Postgres (make dev-db) and fail if it is missing
test: loro
	KB_TEST_REQUIRE_DB=1 go test -race -count=1 ./...

## test-short: unit tests only (database-backed tests skip when Postgres is absent)
test-short: loro
	go test -short -count=1 ./...

## web: build the web app into internal/web/dist (embedded by `make build`)
web:
	cd web && npm ci && npm run build

## e2e: browser tests against bin/kb and the local test Postgres (see web/scripts/e2e-server.mjs)
e2e: build
	cd web && npx playwright test

## docker: build the container image
docker:
	docker build -t kb:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

## dev-env: print an environment for running against the local Postgres cluster
dev-env:
	@cat deploy/compose/.env.example

clean:
	rm -rf bin dist
