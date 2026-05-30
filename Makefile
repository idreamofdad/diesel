# mautrix-go's crypto layer defaults to libolm (a C library); the "goolm"
# build tag swaps in the pure-Go implementation so the build doesn't need
# olm/olm.h installed. Diesel uses Matrix's E2EE with goolm to keep the
# system-dep surface as small as possible.
# GOFLAGS as an environment variable needs each flag in -flag=value form
# (space-separated "-tags goolm" is rejected as a stray non-flag).
GOFLAGS := -tags=goolm
export GOFLAGS

.PHONY: build desktop daemon lint test vet

# Build everything. The desktop app needs cgo (Fyne + native audio); the
# default toolchain has CGO enabled, so this covers both binaries.
build:
	go build ./...

# Desktop app: native window + native audio. Requires cgo.
desktop:
	@mkdir -p bin
	go build -o bin/diesel ./cmd/diesel

# Headless daemon: hub + HTTP server (web UI) + bridges. No window, no
# native audio (the browser does capture/VAD/STT/TTS). Fully cgo-free, so it
# cross-compiles and ships as a single static binary.
daemon:
	@mkdir -p bin
	CGO_ENABLED=0 go build -o bin/dieseld ./cmd/dieseld

lint:
	golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 --build-tags goolm ./...

test:
	go test ./...

vet:
	go vet ./...
