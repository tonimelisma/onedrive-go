package devtool

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os/user"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	asUserTestName     = "dev"
	asUserTestUID      = 1001
	asUserTestGID      = 1002
	asUserTestHome     = "/home/dev"
	asUserTestGoBinary = "/usr/local/go/bin/go"
	asUserTestGoPath   = "/root/go"
	asUserTestModCache = "/root/go/pkg/mod"
	asUserTestGoRoot   = "/root/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.11.linux-amd64"
)

// chownCall records one ownership hand-over.
type chownCall struct {
	path string
	uid  int
	gid  int
}

// fakeTestUserHost is a Linux host whose effective uid, user database and
// installed tools a test chooses.
type fakeTestUserHost struct {
	euid   int
	tools  []string
	chowns []chownCall
}

func (h *fakeTestUserHost) host() testUserHost {
	return testUserHost{
		goos: linuxGOOS,
		euid: h.euid,
		currentUser: func() (*user.User, error) {
			if h.euid == rootUID {
				return &user.User{Username: "root", Uid: "0", Gid: "0", HomeDir: "/root"}, nil
			}
			return &user.User{Username: asUserTestName, Uid: "1001", Gid: "1002", HomeDir: asUserTestHome}, nil
		},
		lookupUser: func(name string) (*user.User, error) {
			if name != asUserTestName {
				return nil, user.UnknownUserError(name)
			}
			return &user.User{Username: asUserTestName, Uid: "1001", Gid: "1002", HomeDir: asUserTestHome}, nil
		},
		lookPath: func(name string) (string, error) {
			if name == goCommandName {
				return asUserTestGoBinary, nil
			}
			if slices.Contains(h.tools, name) {
				return "/usr/bin/" + name, nil
			}
			return "", fs.ErrNotExist
		},
		chown: func(path string, uid, gid int) error {
			h.chowns = append(h.chowns, chownCall{path: path, uid: uid, gid: gid})
			return nil
		},
	}
}

func goEnvRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string][]byte{
		"go env GOPATH GOMODCACHE GOROOT": []byte(asUserTestGoPath + "\n" + asUserTestModCache + "\n" + asUserTestGoRoot + "\n"),
	}}
}

func envValuesOf(env []string, key string) []string {
	var values []string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			values = append(values, value)
		}
	}

	return values
}

// Validates: R-6.10.38
func TestWithTestUser_RunsGoTestAsTheUserThroughRunuser(t *testing.T) {
	t.Parallel()

	host := &fakeTestUserHost{euid: rootUID, tools: []string{"runuser", "setpriv", "su"}}
	inner := goEnvRunner()
	var stderr bytes.Buffer
	runner, err := withTestUser(inner, asUserTestName, host.host(), &stderr)
	require.NoError(t, err)

	rootEnv := []string{"HOME=/root", "PATH=/usr/bin", "GOCACHE=/root/.cache/go-build", "GOTOOLCHAIN=go1.26.11"}
	require.NoError(t, runner.Run(t.Context(), "/repo", rootEnv, &bytes.Buffer{}, &bytes.Buffer{}, "go", "build", "./..."))
	require.NoError(t, runner.Run(t.Context(), "/repo", rootEnv, &bytes.Buffer{}, &bytes.Buffer{}, "go", "test", "-short", "./..."))
	_, err = runner.Output(t.Context(), "/repo", rootEnv, "go", "test", "-json", "./internal/sync")
	require.NoError(t, err)

	require.Len(t, inner.runCommands, 2)
	assert.Equal(t, "go", inner.runCommands[0].name, "a command other than go test runs as it is")
	assert.Equal(t, rootEnv, inner.runCommands[0].env)

	test := inner.runCommands[1]
	assert.Equal(t, "/usr/bin/runuser", test.name)
	assert.Equal(t, []string{"-u", asUserTestName, "--", asUserTestGoBinary, "test", "-short", "./..."}, test.args)
	assert.Equal(t, []string{asUserTestHome}, envValuesOf(test.env, "HOME"))
	assert.Equal(t, []string{asUserTestHome + "/.cache/go-build"}, envValuesOf(test.env, "GOCACHE"),
		"the user writes its own build cache, never root's")
	assert.Equal(t, []string{asUserTestModCache}, envValuesOf(test.env, "GOMODCACHE"))
	assert.Equal(t, []string{asUserTestGoPath}, envValuesOf(test.env, "GOPATH"))
	assert.Equal(t, []string{"go1.26.11"}, envValuesOf(test.env, "GOTOOLCHAIN"), "the pinned toolchain carries over")

	switched := inner.outputCommands[len(inner.outputCommands)-1]
	assert.Equal(t, "/usr/bin/runuser", switched.name)

	note := stderr.String()
	assert.Equal(t, 1, strings.Count(note, "running go test as dev"), "the note prints once")
	for _, want := range []string{"/repo", "GOMODCACHE=" + asUserTestModCache, "GOPATH=" + asUserTestGoPath, "GOROOT=" + asUserTestGoRoot, "GOCACHE=" + asUserTestHome} {
		assert.Contains(t, note, want)
	}
}

// Validates: R-6.10.38
func TestWithTestUser_FallsBackToSetprivThenSu(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		tool     string
		wantArgs []string
	}{
		{
			tool:     "setpriv",
			wantArgs: []string{"--reuid=1001", "--regid=1002", "--init-groups", "--", asUserTestGoBinary, "test", "./..."},
		},
		{
			tool:     "su",
			wantArgs: []string{"-s", "/bin/sh", "-c", `exec "$0" "$@"`, asUserTestName, asUserTestGoBinary, "test", "./..."},
		},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()

			host := &fakeTestUserHost{euid: rootUID, tools: []string{tc.tool}}
			inner := goEnvRunner()
			runner, err := withTestUser(inner, asUserTestName, host.host(), &bytes.Buffer{})
			require.NoError(t, err)

			require.NoError(t, runner.Run(t.Context(), "/repo", nil, &bytes.Buffer{}, &bytes.Buffer{}, "go", "test", "./..."))
			require.Len(t, inner.runCommands, 1)
			assert.Equal(t, "/usr/bin/"+tc.tool, inner.runCommands[0].name)
			assert.Equal(t, tc.wantArgs, inner.runCommands[0].args)
			assert.Equal(t, []string{asUserTestHome}, envValuesOf(inner.runCommands[0].env, "HOME"))
		})
	}
}

// Validates: R-6.10.38
func TestWithTestUser_HandsTheCoverageProfileToTheUser(t *testing.T) {
	t.Parallel()

	host := &fakeTestUserHost{euid: rootUID, tools: []string{"runuser"}}
	inner := goEnvRunner()
	runner, err := withTestUser(inner, asUserTestName, host.host(), &bytes.Buffer{})
	require.NoError(t, err)

	require.NoError(t, runner.Run(t.Context(), "/repo", nil, &bytes.Buffer{}, &bytes.Buffer{},
		"go", "test", "-race", "-coverprofile=/tmp/cover.out", "./..."))
	assert.Equal(t, []chownCall{{path: "/tmp/cover.out", uid: asUserTestUID, gid: asUserTestGID}}, host.chowns)
}

// Validates: R-6.10.38
func TestWithTestUser_WarnsOnceAsRootWithoutAUser(t *testing.T) {
	t.Parallel()

	host := &fakeTestUserHost{euid: rootUID, tools: []string{"runuser"}}
	inner := &fakeRunner{}
	var stderr bytes.Buffer
	runner, err := withTestUser(inner, "", host.host(), &stderr)
	require.NoError(t, err)

	require.NoError(t, runner.Run(t.Context(), "/repo", nil, &bytes.Buffer{}, &bytes.Buffer{}, "go", "build", "./..."))
	assert.Empty(t, stderr.String(), "a step without go test says nothing")

	for range 2 {
		require.NoError(t, runner.Run(t.Context(), "/repo", nil, &bytes.Buffer{}, &bytes.Buffer{}, "go", "test", "./..."))
	}
	assert.Equal(t, rootTestWarning, stderr.String())
	assert.Equal(t, "go", inner.runCommands[1].name, "as root without a user, go test runs as root")
}

// Validates: R-6.10.38
func TestWithTestUser_RefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()

	nonRoot := &fakeTestUserHost{euid: asUserTestUID, tools: []string{"runuser"}}
	_, err := withTestUser(&fakeRunner{}, "builder", nonRoot.host(), &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "switching users needs root on Linux")

	same, err := withTestUser(&fakeRunner{}, asUserTestName, nonRoot.host(), &bytes.Buffer{})
	require.NoError(t, err, "running as the named user already needs no switch")
	assert.IsType(t, &fakeRunner{}, same)

	noTool := &fakeTestUserHost{euid: rootUID}
	_, err = withTestUser(&fakeRunner{}, asUserTestName, noTool.host(), &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none of runuser, setpriv, su is installed")

	root := &fakeTestUserHost{euid: rootUID, tools: []string{"runuser"}}
	_, err = withTestUser(&fakeRunner{}, "nobody-here", root.host(), &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "look up user")
}

// Validates: R-6.10.38
func TestWithTestUser_FailsTheGoTestWhenGoEnvFails(t *testing.T) {
	t.Parallel()

	host := &fakeTestUserHost{euid: rootUID, tools: []string{"runuser"}}
	inner := &fakeRunner{outputErr: errors.New("go: toolchain download refused")}
	runner, err := withTestUser(inner, asUserTestName, host.host(), &bytes.Buffer{})
	require.NoError(t, err)

	err = runner.Run(t.Context(), "/repo", nil, &bytes.Buffer{}, &bytes.Buffer{}, "go", "test", "./...")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read go env")
	assert.Empty(t, inner.runCommands, "no go test runs with a guessed module cache")
}

// Validates: R-6.10.38
func TestRunVerifyQuickRunsGoTestAsTheTestUser(t *testing.T) {
	t.Parallel()

	host := &fakeTestUserHost{euid: rootUID, tools: []string{"runuser"}}
	hostValue := host.host()
	runner := goEnvRunner()
	runner.runStdoutFunc = func(args []string) string {
		if index := slices.Index(args, asUserTestGoBinary); index >= 0 {
			return passingGoTestOutput(args[index+1:])
		}
		return passingGoTestOutput(args)
	}
	var stderr bytes.Buffer

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:         t.TempDir(),
		Profile:          VerifyQuick,
		ShortUnitTimeout: testShortUnitTimeout,
		AsUser:           asUserTestName,
		Stdout:           &bytes.Buffer{},
		Stderr:           &stderr,
		testUserHost:     &hostValue,
	})
	require.NoError(t, err)

	unit := runner.runCommands[len(runner.runCommands)-1]
	assert.Equal(t, "/usr/bin/runuser", unit.name)
	assert.Equal(t, []string{"-u", asUserTestName, "--", asUserTestGoBinary, "test", "-short", testShortUnitTimeoutArg, "./..."}, unit.args)
	for _, command := range runner.runCommands[:len(runner.runCommands)-1] {
		assert.NotEqual(t, "/usr/bin/runuser", command.name, "only go test switches user: %v", command.args)
	}
	assert.Contains(t, stderr.String(), "running go test as dev")
}
