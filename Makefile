APP_NAME    := airc
MODULE      := github.com/babs/airc
PKG         := .
VERSION     ?= v0.0.0
COMMIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null || echo "0000000")
BUILD_TS    := $(shell date -Iseconds)
BUILDER     := $(shell go version 2>/dev/null || echo "unknown")
PROJECT_URL ?= https://$(MODULE)
LDFLAGS     := -s -w \
  -X 'main.Version=$(VERSION)' \
  -X 'main.CommitHash=$(COMMIT_HASH)' \
  -X 'main.BuildTimestamp=$(BUILD_TS)' \
  -X 'main.Builder=$(BUILDER)' \
  -X 'main.ProjectURL=$(PROJECT_URL)'

.PHONY: build run test cover lint clean ui-check

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) $(PKG)

run: build
	./bin/$(APP_NAME)

test: ui-check lint
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.txt ./... && go tool cover -func=coverage.txt | tail -1

lint:
	golangci-lint run

clean:
	rm -rf bin/ coverage.txt

# The UI is one hand-written file with no build step: a syntax error kills the whole page
# silently. Parse every inline script with node when it is available.
ui-check:
	@if command -v node >/dev/null; then node -e "const h=require('fs').readFileSync('pkg/web/ui/index.html','utf8');for(const [i,m] of [...h.matchAll(/<script>([\\s\\S]*?)<\\/script>/g)].entries()){try{new Function(m[1])}catch(e){console.error('index.html script',i,e.message);process.exit(1)}}"; fi
