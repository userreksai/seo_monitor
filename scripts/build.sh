#!/usr/bin/env sh
set -eu

go mod download
go test ./...
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/seo-monitor ./cmd/server
