#!/usr/bin/env bash
# PackCheck - full verification on Linux/macOS.
# Exits non-zero when any gate fails.
set -eu
cd "$(dirname "$0")/.."

echo '== gofmt =='
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then echo "Unformatted files: $unformatted"; exit 1; fi

echo '== go vet =='
go vet ./...

echo '== go test (unit + E2E integration) =='
go test ./... -count=1

echo '== evidence batch (real CLI) =='
go build -o packcheck ./cmd/packcheck
./packcheck seed --db evidence/packcheck.db
./packcheck runbatch --db evidence/packcheck.db --out evidence

echo '== analyzers (optional, when installed) =='
for tool in staticcheck gosec govulncheck; do
  if [ -x "$HOME/go/bin/$tool" ]; then
    echo "-- $tool"
    "$HOME/go/bin/$tool" ./...
  else
    echo "-- $tool not installed; skipped without faking a result"
  fi
done

echo 'FULL VERIFICATION: OK'
