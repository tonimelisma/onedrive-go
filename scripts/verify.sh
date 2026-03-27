#!/usr/bin/env bash

set -euo pipefail

coverage_file="${COVERAGE_FILE:-/tmp/onedrive-go-cover.out}"
coverage_threshold="${COVERAGE_THRESHOLD:-75.0}"

echo "==> golangci-lint"
golangci-lint run --allow-parallel-runners

echo "==> go build"
go build ./...

echo "==> go test -race -coverprofile"
go test -race -coverprofile="${coverage_file}" ./...

echo "==> coverage"
coverage_report="$(go tool cover -func="${coverage_file}")"
echo "${coverage_report}"

coverage_total="$(
	echo "${coverage_report}" |
	awk '/^total:/ {gsub(/%/, "", $3); print $3}'
)"

awk -v actual="${coverage_total}" -v minimum="${coverage_threshold}" '
BEGIN {
	if ((actual + 0) < (minimum + 0)) {
		exit 1
	}
}
' || {
	echo "coverage gate failed: ${coverage_total}% < ${coverage_threshold}%"
	exit 1
}

if [[ -n "${ONEDRIVE_ALLOWED_TEST_ACCOUNTS:-}" && -n "${ONEDRIVE_TEST_DRIVE:-}" ]]; then
	echo "==> go test -tags=e2e"
	go test -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...
else
	echo "==> skipping e2e (credentials not configured)"
fi
