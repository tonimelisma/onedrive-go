package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// TestUserEnv names the environment variable that gives `--as-user` its
// default, so a cloud environment can set it once for every session.
const TestUserEnv = "DEVTOOL_TEST_USER"

const (
	linuxGOOS = "linux"
	rootUID   = 0

	coverProfileFlag = "-coverprofile="

	// rootTestWarning is printed once, before the first go test step, when
	// verify runs as root with no test user.
	rootTestWarning = "devtool verify: running go test as root; tests that rely on file permissions " +
		"(or that refuse root) may fail. Pass --as-user <name> or set " + TestUserEnv + " to run them as that user.\n"
)

// userSwitchTools are the commands that can start a process as another user,
// in the order verify prefers them: runuser and setpriv come with util-linux
// and ask for no password as root; su is the fallback every system has.
var userSwitchTools = []string{"runuser", "setpriv", "su"} //nolint:gochecknoglobals // fixed preference order, never mutated

// testUserHost is what choosing the test user reads from the host, so a test
// can stand in for root, another platform, or a system without runuser.
type testUserHost struct {
	goos        string
	euid        int
	currentUser func() (*user.User, error)
	lookupUser  func(string) (*user.User, error)
	lookPath    func(string) (string, error)
	chown       func(string, int, int) error
}

func defaultTestUserHost() testUserHost {
	return testUserHost{
		goos:        runtime.GOOS,
		euid:        os.Geteuid(),
		currentUser: user.Current,
		lookupUser:  user.Lookup,
		lookPath:    exec.LookPath,
		chown:       os.Chown,
	}
}

// withTestUser returns the runner verify's steps use. As root on Linux with a
// test user, it is a runner that starts every go test as that user; as root
// without one, it prints rootTestWarning before the first go test. Elsewhere
// it is runner itself: a test user that is the current user needs no switch,
// and any other one is refused, since only root can switch users.
func withTestUser(runner commandRunner, asUser string, host testUserHost, stderr io.Writer) (commandRunner, error) {
	if host.goos != linuxGOOS || host.euid != rootUID {
		if asUser == "" {
			return runner, nil
		}
		current, err := host.currentUser()
		if err != nil {
			return nil, fmt.Errorf("--as-user %s: read current user: %w", asUser, err)
		}
		if current.Username == asUser {
			return runner, nil
		}

		return nil, fmt.Errorf("--as-user %s: switching users needs root on Linux", asUser)
	}

	if asUser == "" || asUser == "root" {
		return &testUserRunner{inner: runner, stderr: stderr}, nil
	}

	target, err := host.lookupUser(asUser)
	if err != nil {
		return nil, fmt.Errorf("--as-user %s: look up user: %w", asUser, err)
	}
	uid, uidErr := strconv.Atoi(target.Uid)
	gid, gidErr := strconv.Atoi(target.Gid)
	if uidErr != nil || gidErr != nil {
		return nil, fmt.Errorf("--as-user %s: user has non-numeric ids %s:%s", asUser, target.Uid, target.Gid)
	}
	if uid == rootUID {
		return &testUserRunner{inner: runner, stderr: stderr}, nil
	}

	tool, toolPath := "", ""
	for _, candidate := range userSwitchTools {
		if found, lookErr := host.lookPath(candidate); lookErr == nil {
			tool, toolPath = candidate, found
			break
		}
	}
	if tool == "" {
		return nil, fmt.Errorf("--as-user %s: none of %s is installed", asUser, strings.Join(userSwitchTools, ", "))
	}

	return &testUserRunner{
		inner:  runner,
		stderr: stderr,
		target: &testUser{
			name:     target.Username,
			uid:      uid,
			gid:      gid,
			home:     target.HomeDir,
			tool:     tool,
			toolPath: toolPath,
		},
		lookPath: host.lookPath,
		chown:    host.chown,
	}, nil
}

type testUser struct {
	name     string
	uid      int
	gid      int
	home     string
	tool     string
	toolPath string
}

// testUserRunner starts go test commands as the test user and every other
// command as it is. With no target it only prints the root warning.
type testUserRunner struct {
	inner    commandRunner
	stderr   io.Writer
	target   *testUser
	lookPath func(string) (string, error)
	chown    func(string, int, int) error

	// once guards the one-time announcement, and goEnv, which it fills from
	// root's `go env` before the first go test: steps may run concurrently.
	once    sync.Once
	goEnv   testUserGoEnv
	initErr error
}

// testUserGoEnv is what the test user's go test reads from root's setup.
type testUserGoEnv struct {
	goPath   string
	modCache string
	// goRoot is the toolchain go test runs: with GOTOOLCHAIN pinned, the
	// one go.mod names, which go downloaded into the module cache.
	goRoot     string
	goBinary   string
	cacheOwned string
	// trusted are the checkouts git, as the user, must trust: the one go
	// test runs in and, for a worktree nested in another checkout, that one.
	trusted []string
}

func isGoTest(name string, args []string) bool {
	return name == goCommandName && len(args) > 0 && args[0] == goTestSubcommand
}

func (r *testUserRunner) Run(
	ctx context.Context, cwd string, env []string, stdout, stderr io.Writer, name string, args ...string,
) error {
	if !isGoTest(name, args) {
		return r.inner.Run(ctx, cwd, env, stdout, stderr, name, args...) //nolint:wrapcheck // a pass-through runner adds no context
	}
	switchedName, switchedArgs, switchedEnv, err := r.switched(ctx, cwd, env, args)
	if err != nil {
		return err
	}

	return r.inner.Run(ctx, cwd, switchedEnv, stdout, stderr, switchedName, switchedArgs...) //nolint:wrapcheck // as above
}

func (r *testUserRunner) Output(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error) {
	if !isGoTest(name, args) {
		return r.inner.Output(ctx, cwd, env, name, args...) //nolint:wrapcheck // a pass-through runner adds no context
	}
	switchedName, switchedArgs, switchedEnv, err := r.switched(ctx, cwd, env, args)
	if err != nil {
		return nil, err
	}

	return r.inner.Output(ctx, cwd, switchedEnv, switchedName, switchedArgs...) //nolint:wrapcheck // as above
}

func (r *testUserRunner) CombinedOutput(
	ctx context.Context, cwd string, env []string, name string, args ...string,
) ([]byte, error) {
	if !isGoTest(name, args) {
		return r.inner.CombinedOutput(ctx, cwd, env, name, args...) //nolint:wrapcheck // a pass-through runner adds no context
	}
	switchedName, switchedArgs, switchedEnv, err := r.switched(ctx, cwd, env, args)
	if err != nil {
		return nil, err
	}

	return r.inner.CombinedOutput(ctx, cwd, switchedEnv, switchedName, switchedArgs...) //nolint:wrapcheck // as above
}

// switched returns the command, arguments and environment that run go test
// as the test user, or the go test itself, unchanged, with no target.
func (r *testUserRunner) switched(
	ctx context.Context, cwd string, env []string, args []string,
) (string, []string, []string, error) {
	r.once.Do(func() { r.initErr = r.announce(ctx, cwd, env) })
	if r.initErr != nil {
		return "", nil, nil, r.initErr
	}
	if r.target == nil {
		return goCommandName, args, env, nil
	}

	if err := r.handCoverProfileOver(args); err != nil {
		return "", nil, nil, err
	}

	// HOME and GOCACHE are the user's own, so the build cache is one it can
	// write; GOPATH and GOMODCACHE stay root's, so the module cache and the
	// pinned toolchain root already downloaded are reused, read-only.
	userEnv := withEnvValues(env,
		envSetting{key: "HOME", value: r.target.home},
		envSetting{key: "USER", value: r.target.name},
		envSetting{key: "LOGNAME", value: r.target.name},
		envSetting{key: "GOPATH", value: r.goEnv.goPath},
		envSetting{key: "GOMODCACHE", value: r.goEnv.modCache},
		envSetting{key: "GOCACHE", value: r.goEnv.cacheOwned},
	)
	userEnv, err := trustCheckoutForGit(userEnv, r.goEnv.trusted...)
	if err != nil {
		return "", nil, nil, err
	}
	goArgs := append([]string{r.goEnv.goBinary}, args...)

	switch r.target.tool {
	case "runuser":
		return r.target.toolPath, append([]string{"-u", r.target.name, "--"}, goArgs...), userEnv, nil
	case "setpriv":
		return r.target.toolPath, append([]string{
			"--reuid=" + strconv.Itoa(r.target.uid),
			"--regid=" + strconv.Itoa(r.target.gid),
			"--init-groups",
			"--",
		}, goArgs...), userEnv, nil
	default:
		// su hands the words after the user to the shell, which execs them
		// as they are: no quoting of the go test arguments is needed.
		return r.target.toolPath, append([]string{
			"-s", "/bin/sh", "-c", `exec "$0" "$@"`, r.target.name,
		}, goArgs...), userEnv, nil
	}
}

// announce prints, once, the root warning or what the test user needs, and
// reads from root's go env the module cache and GOPATH the user reuses.
func (r *testUserRunner) announce(ctx context.Context, cwd string, env []string) error {
	if r.target == nil {
		if _, err := io.WriteString(r.stderr, rootTestWarning); err != nil {
			return fmt.Errorf("write root test warning: %w", err)
		}

		return nil
	}

	goBinary, err := r.lookPath(goCommandName)
	if err != nil {
		return fmt.Errorf("--as-user %s: find go: %w", r.target.name, err)
	}
	out, err := r.inner.Output(ctx, cwd, env, goCommandName, "env", "GOPATH", "GOMODCACHE", "GOROOT")
	if err != nil {
		return fmt.Errorf("--as-user %s: read go env: %w", r.target.name, err)
	}
	const goEnvLineCount = 3
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != goEnvLineCount {
		return fmt.Errorf("--as-user %s: go env printed %d lines, want %d", r.target.name, len(lines), goEnvLineCount)
	}
	r.goEnv = testUserGoEnv{
		goPath:     strings.TrimSpace(lines[0]),
		modCache:   strings.TrimSpace(lines[1]),
		goRoot:     strings.TrimSpace(lines[2]),
		goBinary:   goBinary,
		cacheOwned: filepath.Join(r.target.home, ".cache", "go-build"),
		trusted:    checkoutsToTrust(ctx, r.inner, cwd, env),
	}

	note := fmt.Sprintf("devtool verify: running go test as %s (uid %d, via %s). %s must be able to read "+
		"the checkout %s (git trusts it through GIT_CONFIG_COUNT), "+
		"read (and reach through every directory above) GOMODCACHE=%s, GOPATH=%s, "+
		"GOROOT=%s and %s, and write its own GOCACHE=%s.\n",
		r.target.name, r.target.uid, r.target.tool, r.target.name, cwd,
		r.goEnv.modCache, r.goEnv.goPath, r.goEnv.goRoot, goBinary, r.goEnv.cacheOwned)
	if _, err := io.WriteString(r.stderr, note); err != nil {
		return fmt.Errorf("write test user note: %w", err)
	}

	return nil
}

// trustCheckoutForGit adds safe.directory=checkout to env as git
// configuration of command scope (GIT_CONFIG_COUNT, GIT_CONFIG_KEY_<n>,
// GIT_CONFIG_VALUE_<n>), after any the caller already set. The checkout is
// root's, and git refuses a repository another user owns, so without it every
// test that runs git in the checkout fails as the test user. Command scope is
// one git honors for safe.directory, and it reaches only this go test's
// processes: nothing is written to the user's git configuration, and no other
// repository is trusted.
func trustCheckoutForGit(env []string, checkouts ...string) ([]string, error) {
	count := 0
	if existing := envValue(env, "GIT_CONFIG_COUNT"); existing != "" {
		parsed, err := strconv.Atoi(existing)
		if err != nil || parsed < 0 {
			return nil, fmt.Errorf("--as-user: GIT_CONFIG_COUNT=%q is not a count of git settings", existing)
		}
		count = parsed
	}
	settings := make([]envSetting, 0, 2*len(checkouts)+1)
	for _, checkout := range checkouts {
		index := strconv.Itoa(count)
		settings = append(settings,
			envSetting{key: "GIT_CONFIG_KEY_" + index, value: "safe.directory"},
			envSetting{key: "GIT_CONFIG_VALUE_" + index, value: checkout})
		count++
	}
	settings = append(settings, envSetting{key: "GIT_CONFIG_COUNT", value: strconv.Itoa(count)})

	return withEnvValues(env, settings...), nil
}

// checkoutsToTrust lists the checkouts git must trust for go test in cwd:
// cwd, and, when cwd is a worktree whose repository another checkout holds,
// that checkout, since git checks the owner of both. Root asks git, before
// the switch; a directory git does not know as a repository has only itself.
func checkoutsToTrust(ctx context.Context, runner commandRunner, cwd string, env []string) []string {
	trusted := []string{cwd}
	out, err := runner.Output(ctx, cwd, env, "git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return trusted
	}
	commonDir := strings.TrimSpace(string(out))
	if filepath.Base(commonDir) != ".git" {
		return trusted
	}
	if holder := filepath.Dir(commonDir); holder != filepath.Clean(cwd) {
		trusted = append(trusted, holder)
	}

	return trusted
}

// handCoverProfileOver gives the test user the coverage profile verify
// created as root, which the user's go test then writes.
func (r *testUserRunner) handCoverProfileOver(args []string) error {
	for _, arg := range args {
		path, ok := strings.CutPrefix(arg, coverProfileFlag)
		if !ok {
			continue
		}
		err := r.chown(path, r.target.uid, r.target.gid)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("--as-user %s: hand over coverage profile %s: %w", r.target.name, path, err)
		}
	}

	return nil
}

// envSetting is one environment variable verify sets for the test user.
type envSetting struct {
	key   string
	value string
}

// withEnvValues returns env with each setting appended and every earlier
// setting of its key dropped; a setting with an empty value leaves the key
// as env has it.
func withEnvValues(env []string, settings ...envSetting) []string {
	out := make([]string, 0, len(env)+len(settings))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		replaced := false
		for _, setting := range settings {
			if setting.key == key && setting.value != "" {
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, entry)
		}
	}
	for _, setting := range settings {
		if setting.value != "" {
			out = append(out, setting.key+"="+setting.value)
		}
	}

	return out
}
