# The Qt cgo bindings (miqt) pull in Homebrew's Qt headers, which require
# a C++17 compiler. Exporting this for every go/golangci-lint invocation
# keeps cgo type-checking from failing on the older default standard.
export CGO_CXXFLAGS := -std=c++17

.PHONY: lint build test vet

lint:
	golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...
