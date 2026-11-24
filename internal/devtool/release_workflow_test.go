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

	cmd := exec.CommandContext(r.t.Context(), "git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %v: %s", args, out)

	return strings.TrimSpace(string(out))
}

// commit adds an empty commit to the current branch and returns its SHA.
func (r *releaseRepo) commit(message string) string {
	r.t.Helper()

	r.git("commit", "--quiet", "--allow-empty", "-m", message)

	return r.git("rev-parse", "HEAD")
}

// version runs the version script for commit against main and returns its
// output lines, or the error text when it refuses.
func (r *releaseRepo) version(commit string, requested string) (map[string]string, string) {
	r.t.Helper()

	script := filepath.Join(repoRootFromDevtoolPackage(r.t), releaseVersionScript)
	args := []string{script, "refs/heads/main", commit}
	if requested != "" {
		args = append(args, requested)
	}
	cmd := exec.CommandContext(r.t.Context(), "bash", args...) //nolint:gosec // the repository's own script under a fixed interpreter
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
// The version script gives the first release v0.1.0, bumps the patch of the
// highest vX.Y.Z tag after that, honours a higher requested version, lets a
// rerun finish the release a commit already has, and skips a commit a newer
// release already contains.
func TestReleaseVersionScript_ChoosesTheNextVersion(t *testing.T) {
	t.Parallel()

	repo := newReleaseRepo(t)
	first := repo.commit("first")

	outputs, refusal := repo.version(first, "")
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("create", "v0.1.0", "true"), outputs, "the first release")

	repo.git("tag", "v0.1.0", first)
	outputs, refusal = repo.version(first, "")
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("finish", "v0.1.0", "true"), outputs, "a rerun finishes the commit's release")

	// Tags that are not vMAJOR.MINOR.PATCH are not releases.
	second := repo.commit("second")
	repo.git("tag", "v9.0", second)
	repo.git("tag", "v9.0.0-rc1", second)
	repo.git("tag", "nightly", second)
	outputs, refusal = repo.version(second, "")
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("create", "v0.1.1", "true"), outputs, "the patch of the highest release tag is bumped")

	outputs, refusal = repo.version(second, "v0.2.0")
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("create", "v0.2.0", "true"), outputs, "a higher requested version wins")

	// Versions compare numerically, not as text.
	repo.git("tag", "v0.9.0", second)
	third := repo.commit("third")
	repo.git("tag", "v0.10.0", third)
	fourth := repo.commit("fourth")
	outputs, refusal = repo.version(fourth, "")
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("create", "v0.10.1", "true"), outputs)

	outputs, refusal = repo.version(second, "")
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("finish", "v0.9.0", "false"), outputs,
		"finishing an older release leaves the newer one latest")

	// fourth was never released (its run waited behind fifth's), and
	// fifth's release contains it.
	fifth := repo.commit("fifth")
	repo.git("tag", "v0.10.1", fifth)
	outputs, refusal = repo.version(fourth, "")
	require.Empty(t, refusal)
	assert.Equal(t, releaseOutputs("skip", "", "false"), outputs,
		"a commit the newest release contains is not released again under a higher version")
}

// Validates: R-6.10.41
//
// The version script refuses a commit off main, a malformed or lower
// requested version, and a requested version for a commit already released.
func TestReleaseVersionScript_Refuses(t *testing.T) {
	t.Parallel()

	repo := newReleaseRepo(t)
	first := repo.commit("first")
	repo.git("tag", "v1.2.3", first)
	second := repo.commit("second")
	repo.git("checkout", "--quiet", "-b", "feature")
	offMain := repo.commit("feature work")
	repo.git("checkout", "--quiet", "main")

	_, refusal := repo.version(offMain, "")
	assert.Contains(t, refusal, "is not on refs/heads/main; only main is released")

	_, refusal = repo.version(second, "1.3.0")
	assert.Contains(t, refusal, "is not vMAJOR.MINOR.PATCH")

	_, refusal = repo.version(second, "v1.2.3")
	assert.Contains(t, refusal, "is not higher than v1.2.3")

	_, refusal = repo.version(second, "v1.2.0")
	assert.Contains(t, refusal, "is not higher than v1.2.3")

	_, refusal = repo.version(first, "v2.0.0")
	assert.Contains(t, refusal, "is already released as v1.2.3")

	_, refusal = repo.version("no-such-commit", "")
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
	triggers := regexp.MustCompile(`(?m)^  (\w+):`).FindAllStringSubmatch(on[1], -1)
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
	firstStep = firstStep[:strings.Index(firstStep, "      - uses:")]
	assert.Contains(t, firstStep, "name: Refuse any ref but main")
	assert.Contains(t, firstStep, `if [ "${REF}" != refs/heads/main ]; then`)
	assert.Contains(t, firstStep, "exit 1")

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
