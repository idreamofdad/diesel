# The Qt cgo bindings (miqt) pull in Homebrew's Qt headers, which require
# a C++17 compiler. Exporting this for every go/golangci-lint invocation
# keeps cgo type-checking from failing on the older default standard.
export CGO_CXXFLAGS := -std=c++17

# mautrix-go's crypto layer defaults to libolm (a C library); the
# "goolm" build tag swaps in the pure-Go implementation so the build
# doesn't need olm/olm.h installed. Diesel uses Matrix's E2EE with
# goolm to keep the system-dep surface as small as possible.
GOFLAGS := -tags goolm
export GOFLAGS

.PHONY: lint build test vet

lint:
	golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 --build-tags goolm ./...

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...
