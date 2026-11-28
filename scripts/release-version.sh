#!/usr/bin/env bash
# Choose the release version of one commit (R-6.10.41).
#
# Usage: release-version.sh <main-ref> <commit>
#
# Run in a clone that holds the whole history of <main-ref> and every tag.
# The release series, MAJOR.MINOR, is the one line of the file VERSION at
# <commit>; a new series starts with an ordinary pull request that edits it.
# Prints three lines for $GITHUB_OUTPUT:
#
#   action=create|finish|skip
#   version=vX.Y.Z           (empty when action=skip)
#   latest=true|false        (whether version is the highest release tag)
#
# - finish: a vX.Y.Z tag already names <commit>; a rerun completes that
#   release instead of claiming a second version.
# - skip:   the newest release tag already contains <commit>, so a newer
#   merge has been released and releasing this one would publish older code
#   under a higher version.
# - create: the highest tag of the series with its patch bumped, or
#   vMAJOR.MINOR.0 when the series has no release yet.
#
# Refuses a commit that is not on <main-ref>, a VERSION that is missing or
# not MAJOR.MINOR, and a series lower than the highest release tag's.
set -euo pipefail

if [ "$#" -ne 2 ]; then
    echo "usage: release-version.sh <main-ref> <commit>" >&2
    exit 2
fi

main_ref=$1
commit=$(git rev-parse --verify --quiet "$2^{commit}") || {
    echo "release-version: $2 is not a commit" >&2
    exit 1
}
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

if ! git merge-base --is-ancestor "$commit" "$main_ref"; then
    echo "release-version: $commit is not on $main_ref; only main is released" >&2
    exit 1
fi

# release_tags prints the vX.Y.Z tags it is given on stdin, lowest first.
release_tags() {
    local tag
    while IFS= read -r tag; do
        if [[ $tag =~ $semver ]]; then
            echo "$tag"
        fi
    done | sort -V
}

existing=$(git tag --points-at "$commit" | release_tags | tail -n 1)
highest=$(git tag --list 'v*' | release_tags | tail -n 1)
if [ -n "$existing" ]; then
    echo "action=finish"
    echo "version=$existing"
    if [ "$existing" = "$highest" ]; then
        echo "latest=true"
    else
        echo "latest=false"
    fi
    exit 0
fi

if [ -n "$highest" ] && git merge-base --is-ancestor "$commit" "$highest^{commit}"; then
    echo "action=skip"
    echo "version="
    echo "latest=false"
    exit 0
fi

series=$(git show "$commit:VERSION" 2> /dev/null | tr -d '[:space:]') || series=
if ! [[ $series =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo "release-version: VERSION at $commit must hold one MAJOR.MINOR line, not '$series'" >&2
    exit 1
fi

in_series=$(git tag --list "v$series.*" | release_tags | tail -n 1)
if [ -n "$in_series" ]; then
    [[ $in_series =~ $semver ]]
    next="v$series.$((BASH_REMATCH[3] + 1))"
else
    next="v$series.0"
fi
if [ -n "$highest" ] && [ "$(printf '%s\n%s\n' "$highest" "$next" | sort -V | tail -n 1)" != "$next" ]; then
    echo "release-version: series $series is lower than the highest release $highest" >&2
    exit 1
fi

echo "action=create"
echo "version=$next"
echo "latest=true"
