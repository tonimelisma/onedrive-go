package devtool

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	releaseWorkflowFile  = ".github/workflows/release.yml"
	releaseVersionScript = "scripts/release-version.sh"
	releaseSmokeScript   = "scripts/release-smoke.sh"
)

// releaseRepo is a throwaway git repository with a main branch, for the
// version script.
type releaseRepo struct {
	t   *testing.T
	dir string
}

func newReleaseRepo(t *testing.T) *releaseRepo {
	t.Helper()

	repo := &releaseRepo{t: t, dir: t.TempDir()}
	repo.git("init", "--quiet", "--initial-branch=main")

	return repo
}

func (r *releaseRepo) git(args ...string) string {
	r.t.Helper()

	cmd := exec.CommandContext(r.t.Context(), "git", args...) //nolint:gosec // git with the test's own arguments
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %v: %s", args, out)

	return strings.TrimSpace(string(out))
}

// commit adds a commit to the current branch whose VERSION holds series and
// returns its SHA.
func (r *releaseRepo) commit(message string, series string) string {
	r.t.Helper()

	require.NoError(r.t, os.WriteFile(filepath.Join(r.dir, "VERSION"), []byte(series+"\n"), 0o600))
	r.git("add", "VERSION")
	r.git("commit", "--quiet", "--allow-empty", "-m", message)

	return r.git("rev-parse", "HEAD")
}

// version runs the version script for commit against main and returns its
// output lines, or the error text when it refuses.
func (r *releaseRepo) version(commit string) (map[string]string, string) {
	r.t.Helper()

	script := filepath.Join(repoRootFromDevtoolPackage(r.t), releaseVersionScript)
	cmd := exec.CommandContext(r.t.Context(), "bash", script, "refs/heads/main", commit) //nolint:gosec // the repository's own script under a fixed interpreter
	cmd.Dir = r.dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, stderr.String()
	}

	outputs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		name, value, ok := strings.Cut(line, "=")
		require.True(r.t, ok, "the script printed %q", line)
		outputs[name] = value
	}

	return outputs, ""
}

func releaseOutputs(action, version, latest string) map[string]string {
	return map[string]string{"action": action, "version": version, "latest": latest}
}

// Validates: R-6.10.41
//
// The version script takes the series from VERSION: the first release of a
// series is vMAJOR.MINOR.0, each later one bumps the patch of the series'
// highest tag, a rerun finishes the release a commit already has, and a
// commit a newer release already contains is skipped.
func TestReleaseVersionScript_ChoosesTheNextVersion(t *testing.T) {
	t.Parallel()

	repo := newReleaseRepo(t)
	first := repo.commit("first", "0.1")

	outputs, refusal := repo.version(first)
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("create", "v0.1.0", "true"), outputs, "the first release of the series")

	repo.git("tag", "v0.1.0", first)
	outputs, refusal = repo.version(first)
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("finish", "v0.1.0", "true"), outputs, "a rerun finishes the commit's release")

	// Tags that are not vMAJOR.MINOR.PATCH are not releases.
	second := repo.commit("second", "0.1")
	repo.git("tag", "v9.0", second)
	repo.git("tag", "v9.0.0-rc1", second)
	repo.git("tag", "nightly", second)
	outputs, refusal = repo.version(second)
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("create", "v0.1.1", "true"), outputs, "the patch of the series' highest tag is bumped")

	// Versions compare numerically, not as text.
	repo.git("tag", "v0.1.9", second)
	third := repo.commit("third", "0.1")
	repo.git("tag", "v0.1.10", third)
	fourth := repo.commit("fourth", "0.1")
	outputs, refusal = repo.version(fourth)
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("create", "v0.1.11", "true"), outputs)

	outputs, refusal = repo.version(second)
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("finish", "v0.1.9", "false"), outputs,
		"finishing an older release leaves the newer one latest")

	// A pull request that edits VERSION starts a new series at .0.
	fifth := repo.commit("fifth", "0.2")
	outputs, refusal = repo.version(fifth)
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("create", "v0.2.0", "true"), outputs, "a new series starts at .0")

	// fourth was never released (its run waited behind sixth's), and
	// sixth's release contains it.
	repo.git("tag", "v0.2.0", fifth)
	outputs, refusal = repo.version(fourth)
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("skip", "", "false"), outputs,
		"a commit the newest release contains is not released again under a higher version")
}

// Validates: R-6.10.41
//
// The version script refuses a commit off main, a missing or malformed
// VERSION, and a series lower than the highest release's.
func TestReleaseVersionScript_Refuses(t *testing.T) {
	t.Parallel()

	repo := newReleaseRepo(t)
	first := repo.commit("first", "1.2")
	repo.git("tag", "v1.2.3", first)
	repo.git("checkout", "--quiet", "-b", "feature")
	offMain := repo.commit("feature work", "1.2")
	repo.git("checkout", "--quiet", "main")

	_, refusal := repo.version(offMain)
	assert.Contains(t, refusal, "is not on refs/heads/main; only main is released")

	for _, series := range []string{"1.3.0", "v1.3", "1", ""} {
		bad := repo.commit("bad series", series)
		_, refusal = repo.version(bad)
		assert.Contains(t, refusal, "must hold one MAJOR.MINOR line", "series %q", series)
	}

	lower := repo.commit("lower series", "1.1")
	_, refusal = repo.version(lower)
	assert.Contains(t, refusal, "series 1.1 is lower than the highest release v1.2.3")

	repo.git("rm", "--quiet", "VERSION")
	repo.git("commit", "--quiet", "-m", "no version file")
	_, refusal = repo.version(repo.git("rev-parse", "HEAD"))
	assert.Contains(t, refusal, "must hold one MAJOR.MINOR line")

	_, refusal = repo.version("no-such-commit")
	assert.Contains(t, refusal, "is not a commit")
}

// smokeStub writes a stand-in for the release binary: it prints the given
// version, and for `sync --watch` checks the paused throwaway configuration,
// then either runs until SIGTERM or exits at once.
func smokeStub(t *testing.T, printedVersion string, watchExits bool) string {
	t.Helper()

	watch := `trap 'exit 0' TERM
while :; do sleep 0.1; done`
	if watchExits {
		watch = `echo "could not start" >&2
exit 1`
	}
	stub := filepath.Join(t.TempDir(), "onedrive-go")
	content := `#!/usr/bin/env bash
if [ "$1" = --version ]; then
  echo "onedrive-go version ` + printedVersion + `"
  exit 0
fi
[ "$1 $2" = "sync --watch" ] || exit 64
grep -q '^paused = true$' "$XDG_CONFIG_HOME/onedrive-go/config.toml" || exit 65
[ -n "$XDG_DATA_HOME" ] && [ -n "$XDG_STATE_HOME" ] || exit 66
` + watch + "\n"
	require.NoError(t, os.WriteFile(stub, []byte(content), 0o700)) //nolint:gosec // a test stub in the test's own temp dir

	return stub
}

func runReleaseSmoke(t *testing.T, binary, version string) (string, error) {
	t.Helper()

	script := filepath.Join(repoRootFromDevtoolPackage(t), releaseSmokeScript)
	cmd := exec.CommandContext(t.Context(), "bash", script, binary, version) //nolint:gosec // the repository's own script under a fixed interpreter
	cmd.Env = append(os.Environ(), "RELEASE_SMOKE_SECONDS=1")
	out, err := cmd.CombinedOutput()

	return string(out), err
}

// Validates: R-6.10.41
//
// The smoke test passes a binary that prints its tag and keeps
// `sync --watch` running on the paused throwaway configuration, and fails
// one that prints another version or whose watch exits.
func TestReleaseSmokeScript_HoldsTheVersionAndTheRunningWatch(t *testing.T) {
	t.Parallel()

	out, err := runReleaseSmoke(t, smokeStub(t, "v1.2.3", false), "v1.2.3")
	require.NoError(t, err, out)
	assert.Contains(t, out, "release-smoke: v1.2.3 prints its version and sync --watch ran 1s")

	out, err = runReleaseSmoke(t, smokeStub(t, "dev", false), "v1.2.3")
	require.Error(t, err)
	assert.Contains(t, out, `--version printed "onedrive-go version dev", want "onedrive-go version v1.2.3"`)

	out, err = runReleaseSmoke(t, smokeStub(t, "v1.2.3", true), "v1.2.3")
	require.Error(t, err)
	assert.Contains(t, out, "sync --watch exited with status 1")
	assert.Contains(t, out, "could not start")
}

// Validates: R-6.10.41
//
// Only release.yml, run from main, releases: it runs on the push CI run of
// main and on dispatch, refuses any other ref first, tags through the API,
// attests, and verifies what it published the way a consumer does. No other
// workflow attests, and no workflow runs on a tag push.
func TestReleaseWorkflow_OnlyMainReleasesAndNothingRunsOnTags(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootFromDevtoolPackage(t)
	files, err := workflowFiles(repoRoot)
	require.NoError(t, err)
	require.Contains(t, files, releaseWorkflowFile)

	tagTrigger := regexp.MustCompile(`(?m)^\s+tags(-ignore)?:`)
	for _, file := range files {
		content, readErr := readFile(filepath.Join(repoRoot, file))
		require.NoError(t, readErr)
		assert.NotRegexp(t, tagTrigger, string(content), "%s runs on tag pushes", file)
		if file != releaseWorkflowFile {
			assert.NotContains(t, string(content), "actions/attest", "%s attests; only %s may", file, releaseWorkflowFile)
		}
	}

	content, err := readFile(filepath.Join(repoRoot, releaseWorkflowFile))
	require.NoError(t, err)
	workflow := string(content)

	on := regexp.MustCompile(`(?ms)^on:\n(.*?)\n\S`).FindStringSubmatch(workflow)
	require.Len(t, on, 2)
	triggers := regexp.MustCompile(`(?m)^ {2}(\w+):`).FindAllStringSubmatch(on[1], -1)
	var names []string
	for _, trigger := range triggers {
		names = append(names, trigger[1])
	}
	assert.Equal(t, []string{"workflow_run", "workflow_dispatch"}, names)
	assert.Contains(t, on[1], "workflows: [CI]")

	for _, condition := range []string{
		"github.event.workflow_run.conclusion == 'success'",
		"github.event.workflow_run.event == 'push'",
		"github.event.workflow_run.head_branch == 'main'",
	} {
		assert.Contains(t, workflow, condition)
	}

	steps := strings.Index(workflow, "    steps:\n")
	require.Positive(t, steps)
	firstStep := workflow[steps:]
	firstStep, _, found := strings.Cut(firstStep, "      - uses:")
	require.True(t, found, "release.yml has no action step")
	assert.Contains(t, firstStep, "name: Refuse any ref but main")
	assert.Contains(t, firstStep, `if [ "${REF}" != refs/heads/main ]; then`)
	assert.Contains(t, firstStep, "exit 1")

	// The version step pipes the script into tee, so without pipefail a
	// refusal reads as success with no version and the release goes on.
	assert.Contains(t, workflow, "set -euo pipefail\n          bash scripts/release-version.sh",
		"a refused version fails its step")

	for _, fragment := range []string{
		"group: release\n  cancel-in-progress: false",
		"head_sha }}",
		"bash scripts/release-version.sh refs/remotes/origin/main",
		"CGO_ENABLED: '0'",
		"-trimpath",
		"internal/cli.version=${VERSION}",
		"bash scripts/release-smoke.sh",
		"actions/attest-build-provenance@",
		`gh api "repos/${GH_REPO}/git/refs" -f "ref=refs/tags/${VERSION}"`,
		"--generate-notes",
		`--signer-workflow "${GH_REPO}/.github/workflows/release.yml"`,
		"--source-ref refs/heads/main",
	} {
		assert.Contains(t, workflow, fragment)
	}
}
