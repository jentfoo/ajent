export GO111MODULE = on

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-s -w -X github.com/jentfoo/ajent/pkg/config.Version=$(VERSION)"

ifneq ($(shell command -v bash),)
test test-all: SHELL := bash
test test-all: .SHELLFLAGS := -o pipefail -c
_FILTER := | grep -v "no test files"
endif

.PHONY: build build-demo clean test test-all test-cover bench fmt-changed lint

build:
	@mkdir -p bin
	go build $(LDFLAGS) -o ./bin/ajent ./

# Build in demo mode: ajent spawns the sibling scripted model server. Both
# binaries land co-located in bin/, since the demo looks next to its executable.
build-demo:
	@mkdir -p bin
	go build -tags demo $(LDFLAGS) -o ./bin/ajent-demo ./
	cd demo && go build -o ../bin/ajent-demosrv ./

PLATFORMS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64

clean:
	rm -rf bin/

test:
	go test -short ./... $(_FILTER) && cd demo && go test -short ./...

test-all:
	go test -race -cover ./... $(_FILTER) && cd demo && go test -race -cover ./...

test-cover:
	go test -race -coverprofile=test.out ./... && go tool cover --html=test.out

bench:
	go test --benchmem -benchtime=20s -bench='Benchmark.*' -run='^$$' ./...

fmt-changed:
	@files=$$( { git diff --name-only --diff-filter=d HEAD -- '*.go'; git ls-files --others --exclude-standard -- '*.go'; } | sort -u); \
	if [ -n "$$files" ]; then \
		gofmt -w $$files; \
	fi

lint: fmt-changed
	golangci-lint run --config=.golangci.yml --timeout=600s && GOOS=linux go vet ./...
	cd demo && golangci-lint run --config=../.golangci.yml --timeout=600s && GOOS=linux go vet ./...
