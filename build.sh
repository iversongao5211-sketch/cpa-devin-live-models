#!/bin/sh
# Build cpa-devin-live-models as a c-shared plugin (.so) for CPA.
# Run inside golang:1.26-bookworm (linux/amd64, CGO on):
#   docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm sh build.sh
set -eu
VERSION="${VERSION:-1.0.0}"
go mod tidy
go vet ./...
go test ./internal/... -count=1
CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o "cpa-devin-live-models-v${VERSION}.so" .
ls -la "cpa-devin-live-models-v${VERSION}.so"*
