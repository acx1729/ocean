# R1 build entry points. The binary has no CLI; these targets are for developers and CI.
SHELL := /bin/bash
export PATH := $(PATH):$(HOME)/go/bin:$(HOME)/.cargo/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/acx1729/ocean/internal/version.Version=$(VERSION) -X github.com/acx1729/ocean/internal/version.Commit=$(COMMIT)
LORO_LIB := rust/loro-cabi/target/release/libloro_cabi.a

.PHONY: all build loro proto lint test test-short web docker dev-env clean tools

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

## test: the full Go suite (needs Postgres; see KB_TEST_DATABASE_URL)
test: loro
	go test -race -count=1 ./...

## test-short: unit tests only
test-short: loro
	go test -short -count=1 ./...

## web: build the web app into internal/web/dist
web:
	cd web && npm ci && npm run build

## docker: build the container image
docker:
	docker build -t kb:$(VERSION) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

## dev-env: print an environment for running against the local Postgres cluster
dev-env:
	@cat deploy/compose/.env.example

clean:
	rm -rf bin dist
