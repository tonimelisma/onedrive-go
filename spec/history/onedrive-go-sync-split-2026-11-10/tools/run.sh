#!/bin/bash
# usage: tools/run.sh <conf-file> <out-dir> [repo]
# Runs the end-state simulator on one configuration and prints its verdict line.
# Set GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 in the environment of a prebuilt
# binary (go build -o endstate-bin tools/endstate/endstate.go) to check the
# macOS file set; `go run` under GOOS=darwin would build the tool for macOS.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
conf="$(realpath "$1")"
out="$(realpath -m "$2")"
repo="${3:-$HOME/src/onedrive-go}"
GOFLAGS=-p=1 nice go -C "$repo" run "$here/endstate/endstate.go" -repo "$repo" -conf "$conf" -out "$out"
