export GO111MODULE = on

ifneq ($(shell command -v bash),)
test test-all: SHELL := bash
test test-all: .SHELLFLAGS := -o pipefail -c
_FILTER := | grep -v "no test files"
endif

.PHONY: build build-demo clean test test-all test-cover bench fmt-changed lint

build:
	@mkdir -p bin
	go build -o ./bin/ajent ./

# Build in demo mode: the scripted stand-in (demo.go) is gated behind the `demo`
# build tag, so it only compiles into this binary.
build-demo:
	@mkdir -p bin
	go build -tags demo -o ./bin/ajent-demo ./

PLATFORMS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64

clean:
	rm -rf bin/

test:
	go test -short ./... $(_FILTER)

test-all:
	go test -race -cover ./... $(_FILTER)

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
