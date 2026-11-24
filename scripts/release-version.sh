#!/usr/bin/env bash
# Choose the release version of one commit (R-6.10.41).
#
# Usage: release-version.sh <main-ref> <commit> [requested-version]
#
# Run in a clone that holds the whole history of <main-ref> and every tag.
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
# - create: the next version, either <requested-version> when it is higher
#   than every release tag, or the highest tag with its patch bumped, or
#   v0.1.0 when there is none.
#
# Refuses a commit that is not on <main-ref>, a malformed or lower requested
# version, and a requested version for a commit that has one already.
set -euo pipefail

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
    echo "usage: release-version.sh <main-ref> <commit> [requested-version]" >&2
    exit 2
fi

main_ref=$1
commit=$(git rev-parse --verify --quiet "$2^{commit}") || {
    echo "release-version: $2 is not a commit" >&2
    exit 1
}
requested=${3:-}
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

if ! git merge-base --is-ancestor "$commit" "$main_ref"; then
    echo "release-version: $commit is not on $main_ref; only main is released" >&2
    exit 1
fi

if [ -n "$requested" ] && ! [[ $requested =~ $semver ]]; then
    echo "release-version: requested version $requested is not vMAJOR.MINOR.PATCH" >&2
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
if [ -n "$existing" ]; then
    if [ -n "$requested" ] && [ "$requested" != "$existing" ]; then
        echo "release-version: $commit is already released as $existing" >&2
        exit 1
    fi
    highest=$(git tag --list 'v*' | release_tags | tail -n 1)
    echo "action=finish"
    echo "version=$existing"
    if [ "$existing" = "$highest" ]; then
        echo "latest=true"
    else
        echo "latest=false"
    fi
    exit 0
fi

highest=$(git tag --list 'v*' | release_tags | tail -n 1)

if [ -n "$highest" ] && git merge-base --is-ancestor "$commit" "$highest^{commit}"; then
    if [ -n "$requested" ]; then
        echo "release-version: $highest already contains $commit" >&2
        exit 1
    fi
    echo "action=skip"
    echo "version="
    echo "latest=false"
    exit 0
fi

if [ -n "$requested" ]; then
    if [ -n "$highest" ] && [ "$(printf '%s\n%s\n' "$highest" "$requested" | sort -V | tail -n 1)" != "$requested" ] ||
        [ "$requested" = "$highest" ]; then
        echo "release-version: requested version $requested is not higher than $highest" >&2
        exit 1
    fi
    next=$requested
elif [ -z "$highest" ]; then
    next=v0.1.0
else
    [[ $highest =~ $semver ]]
    next="v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$((BASH_REMATCH[3] + 1))"
fi

echo "action=create"
echo "version=$next"
echo "latest=true"
