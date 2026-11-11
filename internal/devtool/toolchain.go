package devtool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	goModFileName     = "go.mod"
	goToolchainEnvVar = "GOTOOLCHAIN"

	// goFirstPatchSuffix turns a language version (`go 1.26`) into the name of
	// its first release (`go1.26.0`), the only form GOTOOLCHAIN accepts.
	goFirstPatchSuffix = ".0"
)

var (
	// goModToolchainPattern matches go.mod's `toolchain go1.26.10` directive.
	goModToolchainPattern = regexp.MustCompile(`(?m)^toolchain[ \t]+(go\S+)[ \t]*$`)

	// goModGoVersionPattern matches go.mod's `go 1.26.0` directive.
	goModGoVersionPattern = regexp.MustCompile(`(?m)^go[ \t]+(\d+\.\d+)(\.\d+)?[ \t]*$`)

	// goMinorVersionPattern takes `1.26` out of a toolchain name such as
	// `go1.26.10`.
	goMinorVersionPattern = regexp.MustCompile(`^go(\d+\.\d+)`)
)

// pinnedGoToolchain returns the Go toolchain go.mod pins, as a GOTOOLCHAIN
// value such as `go1.26.10`.
//
// The `toolchain` directive is the pin. The go command drops that line when it
// equals the `go` directive and then treats the `go` version as the toolchain,
// so a go.mod without the line pins that version instead. A tree without a
// go.mod has nothing to pin. A go.mod that names no version at all is an
// error: carrying on unpinned is the drift the pin exists to stop.
func pinnedGoToolchain(repoRoot string) (string, error) {
	data, err := readFile(filepath.Join(repoRoot, goModFileName))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Go toolchain pin: %w", err)
	}

	if m := goModToolchainPattern.FindSubmatch(data); m != nil {
		return string(m[1]), nil
	}
	if m := goModGoVersionPattern.FindSubmatch(data); m != nil {
		patch := string(m[2])
		if patch == "" {
			patch = goFirstPatchSuffix
		}

		return "go" + string(m[1]) + patch, nil
	}

	return "", fmt.Errorf("%s has neither a toolchain nor a go directive to pin the Go toolchain", goModFileName)
}

// withPinnedToolchain returns env with GOTOOLCHAIN set to the pinned toolchain,
// so every go command devtool starts, and the linter that loads the standard
// library through one, uses the toolchain go.mod names whatever Go the host
// has installed.
//
// A GOTOOLCHAIN the caller already set wins: it is how a developer tries a
// newer toolchain on purpose. An empty value counts as unset, as it does for
// the go command. The caller's slice is never modified.
func withPinnedToolchain(env []string, toolchain string) []string {
	if toolchain == "" || envValue(env, goToolchainEnvVar) != "" {
		return env
	}

	pinned := make([]string, 0, len(env)+1)
	pinned = append(pinned, env...)

	return append(pinned, goToolchainEnvVar+"="+toolchain)
}

// envValue returns the value of name in an environment list. The last entry
// wins, as it does for the process that receives the list.
func envValue(env []string, name string) string {
	value := ""
	prefix := name + "="
	for _, entry := range env {
		if rest, ok := strings.CutPrefix(entry, prefix); ok {
			value = rest
		}
	}

	return value
}

// goMinorVersion returns the language version of a toolchain name: `1.26` for
// `go1.26.10`. It returns "" for a name it does not recognize.
func goMinorVersion(toolchain string) string {
	m := goMinorVersionPattern.FindStringSubmatch(toolchain)
	if m == nil {
		return ""
	}

	return m[1]
}
