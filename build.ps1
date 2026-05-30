# build.ps1 - Windows build helper for Diesel
#
# Mirrors the Makefile targets but works in native PowerShell.
# The -tags goolm flag swaps in pure-Go Matrix E2EE so the build
# doesn't require the C libolm library.
#
# Usage:
#   .\build.ps1              # build all packages
#   .\build.ps1 test         # run tests
#   .\build.ps1 vet          # run go vet
#   .\build.ps1 lint         # run golangci-lint (requires it to be installed)
#   .\build.ps1 generate     # build the Svelte web frontend into web/dist
#   .\build.ps1 run          # run the desktop app
#   .\build.ps1 voicecheck   # run the audio pipeline test tool

param(
    [Parameter(Position = 0)]
    [string]$Target = "build"
)

$ErrorActionPreference = "Stop"
$Tags = "goolm"

switch ($Target) {
    "build" {
        Write-Host "Building all packages..." -ForegroundColor Cyan
        go build -tags $Tags ./...
    }
    "test" {
        Write-Host "Running tests..." -ForegroundColor Cyan
        go test -tags $Tags ./...
    }
    "vet" {
        Write-Host "Running go vet..." -ForegroundColor Cyan
        go vet -tags $Tags ./...
    }
    "lint" {
        Write-Host "Running golangci-lint..." -ForegroundColor Cyan
        golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 --build-tags $Tags ./...
    }
    "generate" {
        Write-Host "Building Svelte web frontend..." -ForegroundColor Cyan
        Push-Location web
        npm ci
        npm run build
        Pop-Location
    }
    "run" {
        Write-Host "Starting Diesel..." -ForegroundColor Cyan
        go run -tags $Tags ./cmd/diesel
    }
    "voicecheck" {
        Write-Host "Running voice pipeline check..." -ForegroundColor Cyan
        go run -tags $Tags ./cmd/voicecheck
    }
    default {
        Write-Error "Unknown target: $Target. Valid targets: build, test, vet, lint, generate, run, voicecheck"
        exit 1
    }
}
