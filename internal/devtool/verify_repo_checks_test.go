package devtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnHTTPClientDoOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	badDir := filepath.Join(repoRoot, "internal", "bad")
	require.NoError(t, os.MkdirAll(badDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(badDir, "bad_http.go"),
		[]byte(strings.Join([]string{
			"package bad",
			"",
			"import \"net/http\"",
			"",
			"type wrapper struct {",
			"\tclient *http.Client",
			"}",
			"",
			"func do(req *http.Request, w wrapper) (*http.Response, error) {",
			"\treturn w.client.Do(req)",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http.Client.Do")
	assert.Contains(t, err.Error(), "bad_http.go")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksAllowsApprovedHTTPClientDoBoundary(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	graphDir := filepath.Join(repoRoot, "internal", "graph")
	require.NoError(t, os.MkdirAll(graphDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(graphDir, "client_preauth.go"),
		[]byte(strings.Join([]string{
			"package graph",
			"",
			"import \"net/http\"",
			"",
			"type client struct {",
			"\thttpClient *http.Client",
			"}",
			"",
			"func (c *client) do(req *http.Request) (*http.Response, error) {",
			"\treturn c.httpClient.Do(req)",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	require.NoError(t, runRepoConsistencyChecks(repoRoot))
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnCLIProcessGlobalOutput(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	cliDir := filepath.Join(repoRoot, "internal", "cli")
	require.NoError(t, os.MkdirAll(cliDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(cliDir, "bad_output.go"),
		[]byte(strings.Join([]string{
			"package cli",
			"",
			"import (",
			"\t\"fmt\"",
			"\t\"os\"",
			")",
			"",
			"func badOutput() {",
			"\tfmt.Fprintln(os.Stderr, \"oops\")",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cli output boundary violation")
	assert.Contains(t, err.Error(), "bad_output.go")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnExecCommandContextOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_exec.go", []string{
		"package bad",
		"",
		"import (",
		"\t\"context\"",
		"\t\"os/exec\"",
		")",
		"",
		"func run(ctx context.Context) error {",
		"\treturn exec.CommandContext(ctx, \"echo\", \"nope\").Run()",
		"}",
		"",
	}, "exec.CommandContext")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnExecCommandOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_exec_command.go", []string{
		"package bad",
		"",
		"import \"os/exec\"",
		"",
		"func run() error {",
		"\treturn exec.Command(\"echo\", \"nope\").Run()",
		"}",
		"",
	}, "exec.Command")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnSQLOpenOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_sql.go", []string{
		"package bad",
		"",
		"import \"database/sql\"",
		"",
		"func open() (*sql.DB, error) {",
		"\treturn sql.Open(\"sqlite\", \"file:test.db\")",
		"}",
		"",
	}, "sql.Open")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnSignalNotifyOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_signal.go", []string{
		"package bad",
		"",
		"import (",
		"\t\"os\"",
		"\t\"os/signal\"",
		")",
		"",
		"func watch(ch chan os.Signal) {",
		"\tsignal.Notify(ch)",
		"}",
		"",
	}, "signal.Notify")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnSignalStopOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_signal_stop.go", []string{
		"package bad",
		"",
		"import (",
		"\t\"os\"",
		"\t\"os/signal\"",
		")",
		"",
		"func watch(ch chan os.Signal) {",
		"\tsignal.Stop(ch)",
		"}",
		"",
	}, "signal.Stop")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksFailsOnOSExitOutsideApprovedBoundary(t *testing.T) {
	t.Parallel()

	assertRepoConsistencyRejectsPrivilegedCall(t, "bad_exit.go", []string{
		"package bad",
		"",
		"import \"os\"",
		"",
		"func exitNow() {",
		"\tos.Exit(1)",
		"}",
		"",
	}, "os.Exit")
}

// Validates: R-6.10.5
func TestRunRepoConsistencyChecksIgnoresTestSupportOSExitOutsideProductionRoots(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	testutilDir := filepath.Join(repoRoot, "testutil")
	require.NoError(t, os.MkdirAll(testutilDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(testutilDir, "testenv.go"),
		[]byte(strings.Join([]string{
			"package testutil",
			"",
			"import \"os\"",
			"",
			"func fatal() {",
			"\tos.Exit(1)",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.NoError(t, err)
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnRawOSFilesystemCallInGuardedPackage(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	syncDir := filepath.Join(repoRoot, "internal", "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(syncDir, "bad_fs.go"),
		[]byte(strings.Join([]string{
			"package sync",
			"",
			"import \"os\"",
			"",
			"func badFS(path string) error {",
			"\t_, err := os.Stat(path)",
			"\treturn err",
			"}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "raw os filesystem call detected")
	assert.Contains(t, err.Error(), "bad_fs.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncGraphImport(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	multisyncDir := filepath.Join(repoRoot, "internal", "multisync")
	require.NoError(t, os.MkdirAll(multisyncDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(multisyncDir, "bad_graph.go"),
		[]byte(strings.Join([]string{
			"package multisync",
			"",
			"import \"github.com/tonimelisma/onedrive-go/internal/graph\"",
			"",
			"var _ = graph.ErrNotFound",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync must not import internal/graph")
	assert.Contains(t, err.Error(), "bad_graph.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncGraphImportAlias(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_graph_alias.go"), []string{
		"package multisync",
		"",
		"import g \"github.com/tonimelisma/onedrive-go/internal/graph\"",
		"",
		"var _ = g.ErrNotFound",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multisync must not import internal/graph")
	assert.Contains(t, err.Error(), "bad_graph_alias.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnHasFactsApplyGate(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "sync", "bad_topology_gate.go"), []string{
		"package sync",
		"",
		"func bad(batch ShortcutTopologyBatch) bool {",
		"\tif !batch.HasFacts() {",
		"\t\treturn false",
		"\t}",
		"\treturn true",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ShouldApply")
	assert.Contains(t, err.Error(), "bad_topology_gate.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncShortcutAliasMutation(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_alias_mutation.go"), []string{
		"package multisync",
		"",
		"type session struct{}",
		"func (s session) MoveItem() {}",
		"func (s session) ApplyShortcutAliasMutation() {}",
		"",
		"func bad(s session) {",
		"\ts.MoveItem()",
		"\ts.ApplyShortcutAliasMutation()",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent engine")
	assert.Contains(t, err.Error(), "bad_alias_mutation.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncUnexportedShortcutAliasMutation(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_unexported_alias_mutation.go"), []string{
		"package multisync",
		"",
		"type shortcutAliasMutation struct{}",
		"type parentEngine struct{}",
		"func (p parentEngine) applyShortcutAliasMutation(shortcutAliasMutation) error { return nil }",
		"",
		"func bad(p parentEngine) error {",
		"\treturn p.applyShortcutAliasMutation(shortcutAliasMutation{})",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent engine")
	assert.Contains(t, err.Error(), "bad_unexported_alias_mutation.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncRawShortcutObservationTypes(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_raw_shortcut_facts.go"), []string{
		"package multisync",
		"",
		"import syncengine \"github.com/tonimelisma/onedrive-go/internal/sync\"",
		"",
		"type mirror struct {",
		"\tBatch syncengine.ShortcutTopologyBatch",
		"\tRoot syncengine.ShortcutRootRecord",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "raw parent shortcut observation facts")
	assert.Contains(t, err.Error(), "bad_raw_shortcut_facts.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncChildTopologyStateMapping(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_child_state_mapping.go"), []string{
		"package multisync",
		"",
		"import syncengine \"github.com/tonimelisma/onedrive-go/internal/sync\"",
		"",
		"func bad(state syncengine.ShortcutChildTopologyState) bool {",
		"\treturn state == syncengine.ShortcutChildRetiring",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runner actions")
	assert.Contains(t, err.Error(), "bad_child_state_mapping.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncProtectedReservationSynthesis(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_reservation.go"), []string{
		"package multisync",
		"",
		"import syncengine \"github.com/tonimelisma/onedrive-go/internal/sync\"",
		"",
		"type mountSpec struct {",
		"\tlocalReservations []syncengine.ManagedRootReservation",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "protected-root")
	assert.Contains(t, err.Error(), "bad_reservation.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncShortcutRootStoreWrite(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_parent_store.go"), []string{
		"package multisync",
		"",
		"type parentStore struct{}",
		"func (s parentStore) ApplyShortcutTopology() error { return nil }",
		"",
		"func bad(s parentStore) error {",
		"\treturn s.ApplyShortcutTopology()",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent shortcut-root persistence")
	assert.Contains(t, err.Error(), "bad_parent_store.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncParentSyncDirFilesystemAccess(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_parent_fs.go"), []string{
		"package multisync",
		"",
		"import \"github.com/tonimelisma/onedrive-go/internal/synctree\"",
		"",
		"func bad(path string) error {",
		"\t_, err := synctree.Open(path)",
		"\treturn err",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent sync-dir filesystem")
	assert.Contains(t, err.Error(), "bad_parent_fs.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncLocalpathFilesystemAccessOutsideControlSocket(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_localpath.go"), []string{
		"package multisync",
		"",
		"import lp \"github.com/tonimelisma/onedrive-go/internal/localpath\"",
		"",
		"func bad(path string) error {",
		"\treturn lp.Remove(path)",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "control-socket paths")
	assert.Contains(t, err.Error(), "bad_localpath.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncStateDBPurge(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_state_purge.go"), []string{
		"package multisync",
		"",
		"func RemoveStateDBFiles(string) error { return nil }",
		"",
		"func bad() error {",
		"\treturn RemoveStateDBFiles(\"state.db\")",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "child DB mutation")
	assert.Contains(t, err.Error(), "bad_state_purge.go")
}

// Validates: R-2.4.3, R-2.4.8
func TestRunRepoConsistencyChecksFailsOnMultisyncAutomaticShortcutPersistence(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	writeRepoConsistencyGoSource(t, repoRoot, filepath.Join("internal", "multisync", "bad_child_inventory.go"), []string{
		"package multisync",
		"",
		"func UpdateMountInventory(func(any) error) error { return nil }",
		"",
		"func bad() error {",
		"\treturn UpdateMountInventory(nil)",
		"}",
		"",
	})

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parent topology")
	assert.Contains(t, err.Error(), "bad_child_inventory.go")
}

// Validates: R-2.4.8, R-2.4.10
func TestRunRepoConsistencyChecksFailsOnDuplicatedStatIdentityHelper(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	syncDir := filepath.Join(repoRoot, "internal", "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(syncDir, "stat_identity_linux.go"),
		[]byte(strings.Join([]string{
			"package sync",
			"",
			"func statIdentity() {}",
			"",
		}, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal/synctree")
	assert.Contains(t, err.Error(), "stat_identity_linux.go")
}

// Validates: R-6.2.1
func TestRunRepoConsistencyChecksFailsOnStaleFilterSemanticsWording(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	requirementsDir := filepath.Join(repoRoot, "spec", "requirements")
	require.NoError(t, os.MkdirAll(requirementsDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(requirementsDir, "sync.md"),
		[]byte("Filter settings are per-drive only.\n"),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale filter semantics wording")
	assert.Contains(t, err.Error(), "sync.md")
}

func assertRepoConsistencyRejectsPrivilegedCall(
	t *testing.T,
	filename string,
	source []string,
	want string,
) {
	t.Helper()

	repoRoot := t.TempDir()
	writeRepoConsistencyFixtures(t, repoRoot)

	badDir := filepath.Join(repoRoot, "internal", "bad")
	require.NoError(t, os.MkdirAll(badDir, 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(badDir, filename),
		[]byte(strings.Join(source, "\n")),
		0o600,
	))

	err := runRepoConsistencyChecks(repoRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), want)
	assert.Contains(t, err.Error(), filename)
}

func writeRepoConsistencyGoSource(t *testing.T, repoRoot, relPath string, source []string) {
	t.Helper()

	target := filepath.Join(repoRoot, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(t, os.WriteFile(target, []byte(strings.Join(source, "\n")), 0o600))
}
