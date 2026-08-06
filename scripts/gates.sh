#!/usr/bin/env bash
# Quality gates for e2b-blockdevice. Fast gates by default; --full adds fuzz + bench.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> gofmt"
fmt_out=$(gofmt -l .)
if [[ -n "$fmt_out" ]]; then
  echo "gofmt: files need formatting:" >&2
  echo "$fmt_out" >&2
  exit 1
fi

echo "==> go vet"
go vet ./...

echo "==> go test -race"
go test -race ./...

if [[ "${1:-}" == "--full" ]]; then
  echo "==> fuzz (FuzzDeserialize, 30s)"
  go test -run='^$' -fuzz=FuzzDeserialize -fuzztime=30s ./blockdevice

  echo "==> bench (smoke, 1x)"
  go test -run='^$' -bench=. -benchtime=1x ./blockdevice
fi

echo "ALL GATES PASSED"
