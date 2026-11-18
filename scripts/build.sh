#!/usr/bin/env bash
# Build both waker components into bin/ after checking formatting, the UI,
# vet, and the unit tests.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> gofmt"
fmt=$(gofmt -l cmd internal)
if [ -n "$fmt" ]; then
	echo "gofmt would reformat (run gofmt -w on):" >&2
	printf '%s\n' "$fmt" >&2
	exit 1
fi

echo "==> web UI: tsc + asset copy -> web/"
if ! command -v node >/dev/null || ! command -v npm >/dev/null; then
	echo "node/npm not found — install Node.js >= 20 (https://nodejs.org) to build the dashboard UI" >&2
	exit 1
fi
# web/ is pure build output (gitignored): regenerate it from ui/ every build
# so it can't accumulate stale files and the go:embed only ever sees
# artifacts. It must exist before go vet / go test, since the package embeds it.
rm -rf cmd/wakeserver/web
mkdir -p cmd/wakeserver/web
npm ci --prefix cmd/wakeserver/ui
npm run build --prefix cmd/wakeserver/ui
cp cmd/wakeserver/ui/style.css cmd/wakeserver/ui/icon.svg cmd/wakeserver/ui/manifest.webmanifest cmd/wakeserver/web/
cp -r cmd/wakeserver/ui/templates cmd/wakeserver/web/

echo "==> go vet ./..."
go vet ./...

echo "==> go test ./..."
go test ./...

echo "==> building bin/"
go build -o bin/wakeserver ./cmd/wakeserver
go build -o bin/wakeragent ./cmd/wakeragent
echo "built bin/wakeserver and bin/wakeragent"
