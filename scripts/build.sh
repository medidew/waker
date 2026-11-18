#!/usr/bin/env bash
# Build both waker components into bin/ after checking formatting, vet, and
# the unit tests.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> gofmt"
fmt=$(gofmt -l cmd internal)
if [ -n "$fmt" ]; then
	echo "gofmt would reformat (run gofmt -w on):" >&2
	printf '%s\n' "$fmt" >&2
	exit 1
fi

echo "==> go vet ./..."
go vet ./...

echo "==> go test ./..."
go test ./...

echo "==> building bin/"
go build -o bin/wakeserver ./cmd/wakeserver
go build -o bin/wakeragent ./cmd/wakeragent
echo "built bin/wakeserver and bin/wakeragent"
