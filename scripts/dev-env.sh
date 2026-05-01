#!/usr/bin/env bash
# Source this file to set up full XDG isolation for dev builds.
# Usage: source scripts/dev-env.sh && go run . <command>
#
# Creates isolated config, data, and cache directories under
# /tmp/onedrive-go-dev-$USER/ so dev builds never touch production
# config/token/state/cache data in ~/Library/Application Support/.

DEV_BASE="/tmp/onedrive-go-dev-${USER:-dev}"
export XDG_DATA_HOME="$DEV_BASE/data"
export XDG_CONFIG_HOME="$DEV_BASE/config"
export XDG_CACHE_HOME="$DEV_BASE/cache"

mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME"

echo "Dev environment isolated to $DEV_BASE"
