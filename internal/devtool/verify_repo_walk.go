package devtool

// The folders every repository-rule walk leaves out. Each rule reads the
// repository's own sources, so a folder that holds another checkout, local
// state or third-party code is none of its business; a tree nested in such a
// folder must be read by every rule or by none, never by some.
const (
	verifyGitDir = ".git"
	// verifyTestdataDir is the local test-credential bundle, not Go test data.
	verifyTestdataDir = ".testdata"
	// verifyClaudeDir holds the agent harness's local state, its worktrees
	// (whole other trees) among it.
	verifyClaudeDir      = ".claude"
	verifyNodeModulesDir = "node_modules"
	verifyVendorDir      = "vendor"
)

// skipRepositoryRuleDir reports whether a repository-rule walk leaves out a
// directory of this name, at any depth. It is the one skip set of the
// output-boundary, test-convention, spec-ownership, backlog-id, mutex-doc and
// requirement-traceability walks. It also leaves out defaultVerifyLogDirName,
// where verify keeps the output of failed go test steps: verify's own output,
// which may quote any source, and, written by root, unreadable to the test
// user `--as-user` runs the walks' tests as. Two walks follow the go command's own
// pattern rules instead, on purpose: the e2e tier's list of test files
// (e2eTestFiles) and the lint-pass check (filesNoLintPassBuilds) ask what
// `go test ./e2e/...` and `./...` build, and the go command skips testdata
// and every folder whose name starts with `.` or `_`, the dot folders above
// among them.
func skipRepositoryRuleDir(name string) bool {
	switch name {
	case verifyGitDir, verifyTestdataDir, verifyClaudeDir, verifyNodeModulesDir, verifyVendorDir,
		defaultVerifyLogDirName:
		return true
	default:
		return false
	}
}
