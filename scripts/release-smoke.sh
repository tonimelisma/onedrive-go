#!/usr/bin/env bash
# Smoke-test a release binary before it is published (R-6.10.41).
#
# Usage: release-smoke.sh <binary> <version>
#
# The binary must print exactly "onedrive-go version <version>" for
# --version, and `sync --watch` must still be running RELEASE_SMOKE_SECONDS
# (default 30) after it starts, then stop cleanly on SIGTERM. The watch runs
# against a throwaway configuration whose one drive is paused, so it needs no
# account and touches nothing outside a temporary directory: it loads the
# configuration, opens its data and state directories and waits for a resume,
# which is the start-up a broken release fails in.
set -euo pipefail

if [ "$#" -ne 2 ]; then
    echo "usage: release-smoke.sh <binary> <version>" >&2
    exit 2
fi

binary=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
version=$2
seconds=${RELEASE_SMOKE_SECONDS:-30}

printed=$("$binary" --version)
if [ "$printed" != "onedrive-go version $version" ]; then
    echo "release-smoke: --version printed \"$printed\", want \"onedrive-go version $version\"" >&2
    exit 1
fi

scratch=$(mktemp -d)
watch_pid=
cleanup() {
    if [ -n "$watch_pid" ] && kill -0 "$watch_pid" 2>/dev/null; then
        kill -KILL "$watch_pid" 2>/dev/null || true
    fi
    rm -rf "$scratch"
}
trap cleanup EXIT

mkdir -p "$scratch/config/onedrive-go" "$scratch/sync"
cat >"$scratch/config/onedrive-go/config.toml" <<EOF
["personal:release-smoke@example.invalid"]
sync_dir = "$scratch/sync"
paused = true
EOF

HOME="$scratch" \
    XDG_CONFIG_HOME="$scratch/config" \
    XDG_DATA_HOME="$scratch/data" \
    XDG_STATE_HOME="$scratch/state" \
    XDG_CACHE_HOME="$scratch/cache" \
    "$binary" sync --watch >"$scratch/watch.log" 2>&1 &
watch_pid=$!

# The watch must be alive at every check, the last one included, which comes
# after the whole interval.
elapsed=0
while :; do
    if ! kill -0 "$watch_pid" 2>/dev/null; then
        status=0
        wait "$watch_pid" || status=$?
        watch_pid=
        echo "release-smoke: sync --watch exited with status $status after about ${elapsed}s:" >&2
        cat "$scratch/watch.log" >&2
        exit 1
    fi
    if [ "$elapsed" -ge "$seconds" ]; then
        break
    fi
    sleep 1
    elapsed=$((elapsed + 1))
done

kill -TERM "$watch_pid"
status=0
wait "$watch_pid" || status=$?
watch_pid=
if [ "$status" -ne 0 ]; then
    echo "release-smoke: sync --watch did not stop cleanly on SIGTERM (status $status):" >&2
    cat "$scratch/watch.log" >&2
    exit 1
fi

echo "release-smoke: $version prints its version and sync --watch ran ${seconds}s"
