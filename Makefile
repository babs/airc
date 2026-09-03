APP_NAME    := airc
MODULE      := airc
PKG         := .
VERSION     ?= v0.0.0
COMMIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null || echo "0000000")
BUILD_TS    := $(shell date -Iseconds)
BUILDER     := $(shell go version 2>/dev/null || echo "unknown")
PROJECT_URL ?= $(MODULE)
LDFLAGS     := -s -w \
  -X 'main.Version=$(VERSION)' \
  -X 'main.CommitHash=$(COMMIT_HASH)' \
  -X 'main.BuildTimestamp=$(BUILD_TS)' \
  -X 'main.Builder=$(BUILDER)' \
  -X 'main.ProjectURL=$(PROJECT_URL)'

.PHONY: build run test cover lint clean

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) $(PKG)

run: build
	./bin/$(APP_NAME)

test:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.txt ./... && go tool cover -func=coverage.txt | tail -1

lint:
	golangci-lint run

clean:
	rm -rf bin/ coverage.txt
