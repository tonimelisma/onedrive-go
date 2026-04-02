#!/usr/bin/env bash

set -euo pipefail

profile="${1:-default}"
coverage_threshold="${COVERAGE_THRESHOLD:-76.0}"
coverage_file="${COVERAGE_FILE:-}"
cleanup_coverage_file=false

run_public_checks=false
run_e2e=false
run_e2e_full=false
run_integration=false

case "${profile}" in
default)
	run_public_checks=true
	run_e2e=true
	;;
public)
	run_public_checks=true
	;;
e2e)
	run_e2e=true
	;;
e2e-full)
	run_e2e=true
	run_e2e_full=true
	;;
integration)
	run_integration=true
	;;
*)
	echo "usage: ./scripts/verify.sh [default|public|e2e|e2e-full|integration]" >&2
	exit 1
	;;
esac

if [[ "${run_public_checks}" == "true" && -z "${coverage_file}" ]]; then
	coverage_file="$(mktemp "${TMPDIR:-/tmp}/onedrive-go-cover.XXXXXX")"
	cleanup_coverage_file=true
fi

cleanup() {
	if [[ "${cleanup_coverage_file}" == "true" ]]; then
		rm -f "${coverage_file}"
	fi
}

trap cleanup EXIT

if [[ "${run_public_checks}" == "true" ]]; then
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

	echo "==> docs consistency"
	stale_checks=(
		"RunWatch calls"" RunOnce"
		"retry\\.Reconcile""\\.Delay"
		"RetryTransport\\{Policy: ""Transport\\}"
		"compatibility"" wrapper"
		"migration"" bridge"
	)
	check_paths=(
		spec/design
		internal
		scripts
		.github/workflows
	)

	for pattern in "${stale_checks[@]}"; do
		if rg -n "${pattern}" "${check_paths[@]}" >/tmp/onedrive-go-doc-check.out; then
			echo "stale architecture/documentation phrase detected: ${pattern}"
			cat /tmp/onedrive-go-doc-check.out
			exit 1
		fi
	done

	if rg -n 'graph\.MustNewClient\(' --glob '*.go' -g '!**/*_test.go' . >/tmp/onedrive-go-mustnewclient.out; then
		echo "production MustNewClient call detected"
		cat /tmp/onedrive-go-mustnewclient.out
		exit 1
	fi

	if rg -n 'internal/trustedpath|trustedpath\.' --glob '*.go' . >/tmp/onedrive-go-trustedpath.out; then
		echo "trustedpath usage detected"
		cat /tmp/onedrive-go-trustedpath.out
		exit 1
	fi

	if [[ -e internal/sync/orchestrator.go || -e internal/sync/drive_runner.go ]]; then
		echo "control-plane files resurrected under internal/sync"
		exit 1
	fi

	if [[ -e internal/sync/engine_flow_test_helpers_test.go ]]; then
		echo "sync test shim resurrected"
		exit 1
	fi
fi

if [[ "${run_e2e}" == "true" ]]; then
	echo "==> go test -tags=e2e"
	go test -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...
fi

if [[ "${run_e2e_full}" == "true" ]]; then
	echo "==> go test -tags='e2e e2e_full'"
	E2E_LOG_DIR="${E2E_LOG_DIR:-/tmp/e2e-debug-logs}" \
		go test -tags='e2e e2e_full' -race -v -parallel 5 -timeout=30m ./e2e/...
fi

if [[ "${run_integration}" == "true" ]]; then
	echo "==> go test -tags=integration"
	go test -tags=integration -race -v -timeout=5m ./internal/graph/...
fi
