#!/usr/bin/env bash
# check-oauth2-fork.sh — Compare tonimelisma/oauth2 HEAD with upstream golang.org/x/oauth2 HEAD.
# Prints a summary of how many commits behind the fork is (B-138).
#
# Usage: ./scripts/check-oauth2-fork.sh
set -euo pipefail

FORK_REPO="tonimelisma/oauth2"
UPSTREAM_REPO="golang/oauth2" # mirrors golang.org/x/oauth2 on GitHub

echo "=== OAuth2 Fork Sync Check ==="
echo ""

# Get the latest commit SHA from the fork.
FORK_SHA=$(gh api "repos/${FORK_REPO}/commits/master" --jq '.sha' 2>/dev/null || echo "")
if [ -z "$FORK_SHA" ]; then
    echo "ERROR: Could not fetch fork HEAD from ${FORK_REPO}"
    exit 1
fi

echo "Fork HEAD (${FORK_REPO}):     ${FORK_SHA:0:12}"

# Get the latest commit SHA from upstream.
UPSTREAM_SHA=$(gh api "repos/${UPSTREAM_REPO}/commits/master" --jq '.sha' 2>/dev/null || echo "")
if [ -z "$UPSTREAM_SHA" ]; then
    echo "ERROR: Could not fetch upstream HEAD from ${UPSTREAM_REPO}"
    exit 1
fi

echo "Upstream HEAD (${UPSTREAM_REPO}): ${UPSTREAM_SHA:0:12}"
echo ""

if [ "$FORK_SHA" = "$UPSTREAM_SHA" ]; then
    echo "STATUS: Fork is UP TO DATE with upstream."
else
    # Compare commits.
    BEHIND=$(gh api "repos/${UPSTREAM_REPO}/compare/${FORK_SHA}...${UPSTREAM_SHA}" --jq '.ahead_by' 2>/dev/null || echo "unknown")
    echo "STATUS: Fork is ${BEHIND} commit(s) BEHIND upstream."
    echo "        Review: https://github.com/${UPSTREAM_REPO}/compare/${FORK_SHA:0:12}...master"
fi
