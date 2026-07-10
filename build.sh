#!/usr/bin/env sh
set -eu

PROJECT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$PROJECT_DIR"

if ! command -v go >/dev/null 2>&1; then
  echo "error: Go is not installed or is not in PATH" >&2
  exit 1
fi

if [ ! -f go.mod ]; then
  echo "error: go.mod was not found in $PROJECT_DIR; please upload or clone the complete repository" >&2
  exit 1
fi

echo "==> Downloading Go modules"
go mod download

echo "==> Running tests"
go test ./...

echo "==> Building bin/seo-monitor"
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/seo-monitor ./cmd/server

echo "Build complete: $PROJECT_DIR/bin/seo-monitor"
