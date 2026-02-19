#!/bin/bash
# Poll CI checks and manage merges. This is the ONE AND ONLY way to track CI.
#
# Usage:
#   ./scripts/poll-and-merge.sh --hierarchical           # Watch latest push workflow on current branch
#   ./scripts/poll-and-merge.sh 71              # Poll PR checks, merge, verify post-merge
#   ./scripts/poll-and-merge.sh 71 72 73        # Multiple PRs
#
# Exit codes:
#   0 = success (all checks passed / all PRs merged)
#   1 = CI failure
#   2 = timed out

set -euo pipefail

if [ $# -eq 0 ]; then
    echo "Usage: $0 [--push] | <pr_number> [pr_number ...]"
    exit 1
fi

# --push mode: watch the most recent push workflow on the current branch
if [ "$1" = "--push" ]; then
    branch=$(git branch --show-current)
    echo "Watching latest push workflow on branch '${branch}'..."

    # Find the most recent push workflow run on this branch
    retries=0
    run_id=""
    while [ $retries -lt 12 ]; do
        run_id=$(gh run list --branch "$branch" --event push --limit 1 \
            --json databaseId,status -q '.[0].databaseId' 2>/dev/null)
        if [ -n "$run_id" ]; then
            break
        fi
        retries=$((retries + 1))
        echo "  Waiting for workflow to appear (attempt ${retries}/12)..."
        sleep 10
    done

    if [ -z "$run_id" ]; then
        echo "ERROR: No push workflow found on branch '${branch}'."
        exit 1
    fi

    echo "Watching workflow run ${run_id}..."
    if gh run watch "$run_id" --exit-status; then
        echo "CI PASSED."
        exit 0
    else
        echo "CI FAILED."
        echo "View logs: gh run view ${run_id} --log-failed"
        exit 1
    fi
fi

POLL_INTERVAL=10
MAX_DURATION=${MAX_DURATION:-1800}  # 30 minutes default
MAX_API_RETRIES=5                   # consecutive API errors before giving up
START_TIME=$(date +%s)
FAILED_PRS=()
MERGED_PRS=()
TIMED_OUT_PRS=()
POST_MERGE_FAILED_PRS=()

is_transient_error() {
    local stderr_output="$1"
    # Match known transient GitHub API / network errors
    if echo "$stderr_output" | grep -qiE "i/o timeout|connection reset|connection refused|temporary failure|server error|502|503|504|ETIMEDOUT|ECONNRESET"; then
        return 0
    fi
    return 1
}

# Wait for the post-merge push workflow on main and report its result.
# Args: $1 = PR number (for reporting), $2 = merge commit SHA
wait_for_post_merge_workflow() {
    local pr="$1"
    local merge_sha="$2"

    echo ""
    echo "Waiting for post-merge push workflow on main (PR #${pr}, commit ${merge_sha:0:7})..."

    # Give GitHub a few seconds to trigger the push workflow
    sleep 5

    # Find the workflow run triggered by this merge commit
    local retries=0
    local run_id=""
    while [ $retries -lt 12 ]; do
        # List recent push workflow runs on main, find one matching our commit
        run_id=$(gh run list --branch main --event push --limit 5 --json databaseId,headSha,status \
            -q ".[] | select(.headSha == \"${merge_sha}\") | .databaseId" 2>/dev/null | head -1)

        if [ -n "$run_id" ]; then
            break
        fi

        retries=$((retries + 1))
        echo "  Waiting for push workflow to appear (attempt ${retries}/12)..."
        sleep 10
    done

    if [ -z "$run_id" ]; then
        echo "  WARNING: Could not find push workflow for commit ${merge_sha:0:7}."
        echo "  Falling back to most recent push workflow on main..."
        run_id=$(gh run list --branch main --event push --limit 1 --json databaseId -q '.[0].databaseId' 2>/dev/null)
    fi

    if [ -z "$run_id" ]; then
        echo "  ERROR: No push workflow found on main at all."
        POST_MERGE_FAILED_PRS+=("$pr")
        return
    fi

    echo "  Watching workflow run ${run_id}..."

    # Use gh run watch to block until completion
    if gh run watch "$run_id" --exit-status 2>/dev/null; then
        echo "  Post-merge workflow PASSED for PR #${pr}."
    else
        echo "  Post-merge workflow FAILED for PR #${pr}!"
        echo "  View logs: gh run view ${run_id} --log-failed"
        POST_MERGE_FAILED_PRS+=("$pr")
    fi
}

for pr in "$@"; do
    echo "Polling CI for PR #${pr}..."
    api_errors=0

    while true; do
        # Check overall timeout
        elapsed=$(( $(date +%s) - START_TIME ))
        if [ "$elapsed" -ge "$MAX_DURATION" ]; then
            echo "Timed out after ${elapsed}s waiting for PR #${pr}."
            TIMED_OUT_PRS+=("$pr")
            break
        fi

        # Capture both stdout and stderr separately
        stderr_file=$(mktemp)
        ec=0
        gh pr checks "$pr" 2>"$stderr_file" || ec=$?
        stderr_output=$(cat "$stderr_file")
        rm -f "$stderr_file"

        if [ $ec -eq 0 ]; then
            # All checks passed — merge
            echo "CI passed for PR #${pr}, merging..."
            merge_sha=""
            if gh pr merge "$pr" --squash --delete-branch; then
                echo "PR #${pr} merged."
                MERGED_PRS+=("$pr")
                # Get the merge commit SHA for post-merge tracking
                merge_sha=$(gh pr view "$pr" --json mergeCommit -q '.mergeCommit.oid' 2>/dev/null || true)
            else
                # Merge might fail if already merged
                if gh pr view "$pr" --json state -q '.state' 2>/dev/null | grep -qi "MERGED"; then
                    echo "PR #${pr} was already merged."
                    MERGED_PRS+=("$pr")
                    merge_sha=$(gh pr view "$pr" --json mergeCommit -q '.mergeCommit.oid' 2>/dev/null || true)
                else
                    echo "Failed to merge PR #${pr}."
                    FAILED_PRS+=("$pr")
                fi
            fi

            # Track post-merge push workflow if we have a merge SHA
            if [ -n "$merge_sha" ]; then
                wait_for_post_merge_workflow "$pr" "$merge_sha"
            fi
            break
        elif [ $ec -eq 8 ]; then
            # Checks still pending — reset API error counter and retry
            api_errors=0
            sleep "$POLL_INTERVAL"
        elif is_transient_error "$stderr_output"; then
            # Transient API/network error — retry with backoff
            api_errors=$((api_errors + 1))
            if [ "$api_errors" -ge "$MAX_API_RETRIES" ]; then
                echo "Too many consecutive API errors ($api_errors) for PR #${pr}."
                echo "Last error: $stderr_output"
                FAILED_PRS+=("$pr")
                break
            fi
            backoff=$((POLL_INTERVAL * api_errors))
            echo "Transient API error for PR #${pr} (attempt $api_errors/$MAX_API_RETRIES), retrying in ${backoff}s..."
            sleep "$backoff"
        else
            # Actual CI failure
            echo "CI failed for PR #${pr}."
            if [ -n "$stderr_output" ]; then
                echo "  stderr: $stderr_output"
            fi
            FAILED_PRS+=("$pr")
            break
        fi
    done
done

echo ""
echo "=== Results ==="
[ ${#MERGED_PRS[@]} -gt 0 ] && echo "  Merged: ${MERGED_PRS[*]}"
[ ${#FAILED_PRS[@]} -gt 0 ] && echo "  PR CI failed: ${FAILED_PRS[*]}"
[ ${#TIMED_OUT_PRS[@]} -gt 0 ] && echo "  Timed out: ${TIMED_OUT_PRS[*]}"
[ ${#POST_MERGE_FAILED_PRS[@]} -gt 0 ] && echo "  Post-merge workflow failed: ${POST_MERGE_FAILED_PRS[*]}"

if [ ${#TIMED_OUT_PRS[@]} -gt 0 ]; then
    exit 2
fi
if [ ${#FAILED_PRS[@]} -gt 0 ] || [ ${#POST_MERGE_FAILED_PRS[@]} -gt 0 ]; then
    exit 1
fi
