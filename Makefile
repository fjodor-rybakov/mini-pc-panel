GO ?= go
BIN := bin/s1panel

.PHONY: all build build-libusb ledctl test vet fmt cover clean

all: build

# Default HID backend: hidraw (kernel /dev/hidraw*, needs libhidapi-hidraw0 + cgo)
build:
	CGO_ENABLED=1 $(GO) build -o $(BIN) ./cmd/s1panel

# Spec §1: the tree must also compile with the libusb backend.
# REQUIRED backend for newer panel firmware (under-declared output report —
# kernel hidraw rejects the 4105-byte writes); the Docker build uses this.
build-libusb:
	CGO_ENABLED=1 $(GO) build -tags libusb -o bin/s1panel-libusb ./cmd/s1panel

# Hardware triage: one LED frame straight to the CH340 (no cgo, no config).
ledctl:
	CGO_ENABLED=0 $(GO) build -o bin/ledctl ./cmd/ledctl

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

clean:
	rm -rf bin coverage.out
