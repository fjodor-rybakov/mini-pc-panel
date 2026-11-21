GO ?= go
BIN := bin/s1panel

# Stamped into main.version (-ldflags -X) so the startup log line
# "s1panel <hash>" is the deployment check ("is the new image running?").
GITDESC := $(shell git describe --always --dirty 2>/dev/null || echo unknown)
LDFLAGS := -X main.version=$(GITDESC)

.PHONY: all build build-libusb ledctl test test-race vet fmt cover clean

all: build

# Default HID backend: hidraw (kernel /dev/hidraw*, needs libhidapi-hidraw0 + cgo)
build:
	CGO_ENABLED=1 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/s1panel

# Spec §1: the tree must also compile with the libusb backend.
# REQUIRED backend for newer panel firmware (under-declared output report —
# kernel hidraw rejects the 4105-byte writes); the Docker build uses this.
build-libusb:
	CGO_ENABLED=1 $(GO) build -tags libusb -ldflags "$(LDFLAGS)" -o bin/s1panel-libusb ./cmd/s1panel

# Hardware triage: one LED frame straight to the CH340 (no cgo, no config).
ledctl:
	CGO_ENABLED=0 $(GO) build -o bin/ledctl ./cmd/ledctl

test:
	$(GO) test ./...

# Race detector: the display worker, the tick loop and the editor handlers
# share App state and the redraw queue; -race is how the locked/unlocked
# config contract stays enforced.
test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

clean:
	rm -rf bin coverage.out
