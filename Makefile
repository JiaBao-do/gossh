# gossh Makefile - works on Linux, macOS and Windows (GNU make + Git Bash/sh).
#
#   make              build for this machine into bin/
#   make run ARGS=list
#   make sandbox      run against ./.sandbox instead of your real ~/.ssh
#   make debug        start a headless Delve server on :2345 (attach from your IDE)
#   make install      build and install locally, adding gossh to PATH
#   make dist         cross-compile release binaries into dist/

BINARY  := gossh
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
ARGS    ?=
DLV_PORT ?= 2345

ifeq ($(OS),Windows_NT)
  EXT := .exe
  INSTALL_CMD = powershell -NoProfile -ExecutionPolicy Bypass -File ./install.ps1
else
  EXT :=
  INSTALL_CMD = sh ./install.sh
endif

BIN       := bin/$(BINARY)$(EXT)
DEBUG_BIN := bin/$(BINARY)-debug$(EXT)
SANDBOX   := $(CURDIR)/.sandbox

PLATFORMS := windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all build run sandbox debug debug-build tools test vet fmt tidy upgrade \
        dist install uninstall clean help

all: build

help:
	@sed -n 's/^#  //p' Makefile

## --- Local development -------------------------------------------------

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .
	@echo "built $(BIN) ($(VERSION))"

run:
	go run -ldflags "-X main.version=$(VERSION)" . $(ARGS)

# Runs with HOME pointed at ./.sandbox so ~/.ssh/config and ~/.ssh/keys are throwaway.
sandbox: build
	@mkdir -p "$(SANDBOX)"
	HOME="$(SANDBOX)" USERPROFILE="$(SANDBOX)" ./$(BIN) $(ARGS)

## --- Debugging ---------------------------------------------------------

# Unoptimised binary with full debug info, for dlv/IDE "attach" or "exec".
debug-build:
	go build -gcflags "all=-N -l" -ldflags "-X main.version=$(VERSION)-debug" -o $(DEBUG_BIN) .
	@echo "built $(DEBUG_BIN)"

# The TUI owns the terminal, so Delve runs headless; attach with
# "dlv connect :$(DLV_PORT)" or your IDE's "Go Remote" config on port $(DLV_PORT).
debug: debug-build
	dlv exec --headless --listen=:$(DLV_PORT) --api-version=2 --accept-multiclient ./$(DEBUG_BIN) -- $(ARGS)

tools:
	go install github.com/go-delve/delve/cmd/dlv@latest

## --- Quality -----------------------------------------------------------

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

upgrade:
	go get -u ./...
	go mod tidy

## --- Release / install -------------------------------------------------

dist:
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; \
		[ "$$os" = windows ] && ext=.exe; \
		out=dist/$(BINARY)-$$os-$$arch$$ext; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
	done
	@cp install.sh install.ps1 dist/
	@cd dist && (sha256sum gossh-* 2>/dev/null || shasum -a 256 gossh-*) > checksums.txt
	@echo "release files in dist/"

install: build
	GOSSH_BINARY="$(CURDIR)/$(BIN)" $(INSTALL_CMD)

uninstall:
	GOSSH_UNINSTALL=1 $(INSTALL_CMD)

clean:
	go clean
	rm -rf bin dist .sandbox $(BINARY) $(BINARY).exe
