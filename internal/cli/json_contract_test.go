package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// jsonContract is what a command promises under --json.
type jsonContract int

const (
	// jsonDocument: stdout is exactly one JSON value.
	jsonDocument jsonContract = iota + 1
	// jsonLines: stdout is one JSON object per line (streaming commands).
	jsonLines
	// jsonRejected: the command refuses --json with a usage error (exit 2)
	// before doing anything.
	jsonRejected
)

// jsonContractCase runs one leaf command with --json against fakes and
// returns what it wrote to stdout. invoke must execute the leaf taken from
// the real command tree (see runDetachedLeaf), not a freshly built copy, so
// the test proves the wiring users actually get.
//
// schema is the Go type of what invoke's run prints (each line, for
// jsonLines); otherSchemas are the types the command prints in runs invoke
// does not make, such as get and put of a folder rather than a file. Together
// they declare every type the command prints, which the empty-list tests
// (json_lists_test.go) hold to the rule that no list prints null.
type jsonContractCase struct {
	contract     jsonContract
	schema       jsonSchema
	otherSchemas []jsonSchema
	invoke       func(t *testing.T, leaf *cobra.Command) (stdout string, err error)
}

// TestCommandTree_EveryLeafHonorsJSON walks the whole command tree and
// proves every runnable command either prints valid JSON on stdout under
// --json or rejects the flag. --json is a global flag, so a command that
// silently ignored it would print prose to a script expecting JSON; adding a
// command without a case here fails this test.
//
// Validates: R-6.6.33
func TestCommandTree_EveryLeafHonorsJSON(t *testing.T) {
	cases := jsonContractCases()

	seen := map[string]bool{}
	for _, leaf := range runnableLeaves(newRootCmdWithWriters(nil, nil)) {
		path := leaf.CommandPath()
		seen[path] = true

		tc, ok := cases[path]
		if !assert.Truef(t, ok, "command %q has no --json contract case: add one to jsonContractCases "+
			"(print one JSON document or JSON lines on stdout, or reject --json with a usage error)", path) {
			continue
		}

		t.Run(path, func(t *testing.T) {
			setTestDriveHome(t)
			ctx, cancel := context.WithTimeout(t.Context(), fakeGraphTimeout)
			defer cancel()
			leaf := findLeaf(t, path)
			leaf.SetContext(ctx)

			stdout, err := tc.invoke(t, leaf)
			assertJSONContract(t, tc.contract, tc.schema, stdout, err)
		})
	}

	for path := range cases {
		assert.Truef(t, seen[path], "jsonContractCases has %q, which is not a runnable command", path)
	}
}

func assertJSONContract(t *testing.T, contract jsonContract, schema jsonSchema, stdout string, err error) {
	t.Helper()

	switch contract {
	case jsonDocument:
		require.NoError(t, err)
		requireSingleJSONDocument(t, schema, stdout)
	case jsonLines:
		require.NoError(t, err)
		requireJSONLines(t, schema, stdout)
	case jsonRejected:
		// main maps every usageError to exit status 2.
		var usage *usageError
		require.ErrorAs(t, err, &usage, "--json must be rejected as a usage error")
		assert.Empty(t, stdout, "a rejected --json must not print anything on stdout")
	}
}

// requireSingleJSONDocument checks stdout is exactly one document of the
// schema's type, written as writeJSONDocument writes it: opening with { or [
// as the type says, indented by two spaces, ending in one newline, with no
// list printed as null.
func requireSingleJSONDocument(t *testing.T, schema jsonSchema, stdout string) {
	t.Helper()
	require.NotNil(t, schema.typ, "a --json document case names the type it prints")

	dec := json.NewDecoder(strings.NewReader(stdout))
	var doc any
	require.NoErrorf(t, dec.Decode(&doc), "stdout is not JSON:\n%s", stdout)
	var extra any
	require.ErrorIsf(t, dec.Decode(&extra), io.EOF, "stdout has more than one JSON value:\n%s", stdout)

	opening := "{"
	if schema.typ.Kind() == reflect.Slice {
		opening = "["
	}
	assert.Truef(t, strings.HasPrefix(stdout, opening), "a %s document starts with %s:\n%s", schema.typ, opening, stdout)

	var compact, indented bytes.Buffer
	require.NoError(t, json.Compact(&compact, []byte(stdout)))
	require.NoError(t, json.Indent(&indented, compact.Bytes(), "", "  "))
	assert.Equal(t, indented.String()+"\n", stdout, "a --json document is indented by two spaces and ends in exactly one newline")

	requireMatchesSchema(t, schema, stdout)
	assert.Emptyf(t, nullListsIn(schema.typ, doc, "$"), "a list in a --json document prints [], never null:\n%s", stdout)
}

// requireJSONLines checks every stdout line is one compact JSON object of
// the schema's type ending in a newline, with no list printed as null.
func requireJSONLines(t *testing.T, schema jsonSchema, stdout string) {
	t.Helper()
	require.NotNil(t, schema.typ, "a JSON-lines case names the type of each line")
	require.NotEmpty(t, stdout, "a JSON-lines command must print at least one line")
	assert.True(t, strings.HasSuffix(stdout, "\n"), "every JSON line ends in a newline")

	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		var compact bytes.Buffer
		require.NoErrorf(t, json.Compact(&compact, []byte(line)), "stdout line is not JSON: %q", line)
		assert.Equalf(t, compact.String(), line, "a JSON line is compact")

		var obj map[string]any
		require.NoErrorf(t, json.Unmarshal([]byte(line), &obj), "stdout line is not a JSON object: %q", line)
		requireMatchesSchema(t, schema, line)
		assert.Emptyf(t, nullListsIn(schema.typ, obj, "$"), "a list in a JSON line prints [], never null: %q", line)
	}
}

// requireMatchesSchema decodes text into the schema's type, refusing a field
// the type does not have, so a case cannot name the wrong type.
func requireMatchesSchema(t *testing.T, schema jsonSchema, text string) {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	require.NoErrorf(t, dec.Decode(reflect.New(schema.typ).Interface()), "stdout is not a %s:\n%s", schema.typ, text)
}

func runnableLeaves(root *cobra.Command) []*cobra.Command {
	var leaves []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.Runnable() && c != root {
			leaves = append(leaves, c)
		}
	}
	walk(root)

	return leaves
}

// findLeaf builds a fresh tree, finds the command at path, and detaches it
// so it can execute on its own with a test CLIContext (the root's
// PersistentPreRunE would otherwise build a real one).
func findLeaf(t *testing.T, path string) *cobra.Command {
	t.Helper()

	root := newRootCmdWithWriters(nil, nil)
	args := strings.Fields(path)[1:]
	leaf, _, err := root.Find(args)
	require.NoError(t, err)
	require.Equal(t, path, leaf.CommandPath())
	leaf.Parent().RemoveCommand(leaf)

	return leaf
}

// runDetachedLeaf executes leaf with args under cc (forced into --json mode)
// and returns stdout.
func runDetachedLeaf(t *testing.T, leaf *cobra.Command, cc *CLIContext, args ...string) (string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	cc.Flags.JSON = true
	cc.OutputWriter = &stdout
	cc.StatusWriter = &stderr
	if cc.Logger == nil {
		cc.Logger = slog.New(slog.DiscardHandler)
	}

	leaf.SetContext(context.WithValue(leaf.Context(), cliContextKey{}, cc))
	leaf.SetArgs(args)
	leaf.SetOut(io.Discard)
	leaf.SetErr(io.Discard)
	err := leaf.Execute()
	t.Logf("stderr:\n%s", stderr.String())

	return stdout.String(), err
}

// --- fixtures ---

const jsonContractDrive = "personal:user@example.com"

func jsonContractConfig(t *testing.T) (string, driveid.CanonicalID) {
	t.Helper()

	cid := driveid.MustCanonicalID(jsonContractDrive)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, filepath.Join(t.TempDir(), "OneDrive")))
	seedCatalogAccount(t, cid, func(*config.CatalogAccount) {})
	seedCatalogDrive(t, cid, func(drive *config.CatalogDrive) { drive.RemoteDriveID = rootTestDrive123 })

	return cfgPath, cid
}

func jsonContractConfigContext(t *testing.T) (*CLIContext, driveid.CanonicalID) {
	t.Helper()

	cfgPath, cid := jsonContractConfig(t)
	cc := newCommandContext(&bytes.Buffer{}, cfgPath)

	return cc, cid
}

// jsonContractFileContext is a data-command context backed by a fake drive.
func jsonContractFileContext(t *testing.T) (*CLIContext, *fakeGraphDrive) {
	t.Helper()

	cid := driveid.MustCanonicalID(jsonContractDrive)
	fake := newFakeGraphDrive(t, "0000000drive-123")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.baseURL = srv.URL

	statePath := config.DriveStatePath(cid)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))

	logger := slog.New(slog.DiscardHandler)
	runtime := driveops.NewSessionRuntime(nil, "test-agent", logger)
	runtime.GraphBaseURL = srv.URL
	runtime.TokenSourceFn = func(string, *slog.Logger) (graph.TokenSource, error) {
		return staticTokenSource{}, nil
	}

	return &CLIContext{
		Logger: logger,
		Cfg: &config.ResolvedDrive{
			CanonicalID: cid,
			DriveID:     driveid.New(rootTestDrive123),
		},
		Runtime: runtime,
	}, fake
}

func jsonContractCases() map[string]jsonContractCase {
	cases := map[string]jsonContractCase{}
	for _, group := range []map[string]jsonContractCase{
		accountJSONContractCases(),
		driveJSONContractCases(),
		fileJSONContractCases(),
		recycleBinJSONContractCases(),
	} {
		for path, tc := range group {
			cases[path] = tc
		}
	}

	return cases
}

// accountJSONContractCases: sign-in, status, perf and shared.
func accountJSONContractCases() map[string]jsonContractCase {
	return map[string]jsonContractCase{
		"onedrive-go login": {contract: jsonDocument, schema: schemaOf[loginJSONOutput](), invoke: invokeLoginJSON},
		"onedrive-go logout": {contract: jsonDocument, schema: schemaOf[logoutJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")
			cc, _ := jsonContractConfigContext(t)
			return runDetachedLeaf(t, leaf, cc)
		}},
		"onedrive-go status": {contract: jsonDocument, schema: statusOutputSchema(), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, _ := jsonContractConfigContext(t)
			return runDetachedLeaf(t, leaf, cc)
		}},
		"onedrive-go perf capture": {contract: jsonDocument, schema: schemaOf[synccontrol.PerfCaptureResponse](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			startCLIControlSocket(t, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeOneShot},
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					assert.NoError(t, json.NewEncoder(w).Encode(synccontrol.PerfCaptureResponse{
						OwnerMode: synccontrol.OwnerModeOneShot,
						Result:    perf.CaptureResult{OutputDir: t.TempDir()},
					}))
				})
			cc, _ := jsonContractConfigContext(t)
			return runDetachedLeaf(t, leaf, cc, "--duration=5s")
		}},
		"onedrive-go shared": {contract: jsonDocument, schema: schemaOf[sharedListJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, _ := jsonContractConfigContext(t)
			return runDetachedLeaf(t, leaf, cc)
		}},
	}
}

// driveJSONContractCases: the drive subcommands, pause, resume and sync.
func driveJSONContractCases() map[string]jsonContractCase {
	return map[string]jsonContractCase{
		"onedrive-go drive list": {contract: jsonDocument, schema: schemaOf[driveListJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, _ := jsonContractConfigContext(t)
			return runDetachedLeaf(t, leaf, cc)
		}},
		"onedrive-go drive add": {contract: jsonDocument, schema: schemaOf[driveAddResult](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cid := driveid.MustCanonicalID("business:user@example.com")
			writeTestTokenFile(t, config.DefaultDataDir(), "token_business_user@example.com.json")
			cc := newCommandContext(&bytes.Buffer{}, filepath.Join(t.TempDir(), "config.toml"))
			return runDetachedLeaf(t, leaf, cc, cid.String())
		}},
		"onedrive-go drive remove": {contract: jsonDocument, schema: schemaOf[driveRemoveJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, cid := jsonContractConfigContext(t)
			cc.Flags.Drive = []string{cid.String()}
			return runDetachedLeaf(t, leaf, cc)
		}},
		"onedrive-go trash list": {contract: jsonDocument, schema: schemaOf[trashListJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, cid := jsonContractConfigContext(t)
			cc.Flags.Drive = []string{cid.String()}
			return runDetachedLeaf(t, leaf, cc)
		}},
		"onedrive-go trash restore": {contract: jsonDocument, schema: schemaOf[trashRestoreJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, cid := jsonContractConfigContext(t)
			cc.Flags.Drive = []string{cid.String()}
			id := seedTrashEntry(t, config.DriveStatePath(cid), t.TempDir(), "notes.txt")
			return runDetachedLeaf(t, leaf, cc, id)
		}},
		"onedrive-go trash empty": {contract: jsonDocument, schema: schemaOf[trashEmptyJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, cid := jsonContractConfigContext(t)
			cc.Flags.Drive = []string{cid.String()}
			return runDetachedLeaf(t, leaf, cc, "--confirm")
		}},
		"onedrive-go drive rebind-root": {contract: jsonDocument, schema: schemaOf[driveRebindRootJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, cid := jsonContractConfigContext(t)
			cc.Flags.Drive = []string{cid.String()}
			require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))
			return runDetachedLeaf(t, leaf, cc, "--yes")
		}},
		"onedrive-go drive reset-sync-state": {contract: jsonDocument, schema: schemaOf[driveResetSyncStateJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, cid := jsonContractConfigContext(t)
			cc.Flags.Drive = []string{cid.String()}
			require.NoError(t, os.MkdirAll(filepath.Dir(config.DriveStatePath(cid)), 0o700))
			return runDetachedLeaf(t, leaf, cc, "--yes")
		}},
		"onedrive-go drive search": {contract: jsonDocument, schema: schemaOf[driveSearchJSONOutput](), invoke: invokeDriveSearchJSON},
		"onedrive-go pause": {contract: jsonDocument, schema: schemaOf[pauseJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, _ := jsonContractConfigContext(t)
			return runDetachedLeaf(t, leaf, cc, "2h")
		}},
		"onedrive-go resume": {contract: jsonDocument, schema: schemaOf[resumeJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, cid := jsonContractConfigContext(t)
			require.NoError(t, config.SetDriveKey(cc.CfgPath, cid, "paused", "true"))
			return runDetachedLeaf(t, leaf, cc)
		}},
		"onedrive-go sync": {contract: jsonLines, schema: schemaOf[syncJSONEvent](), invoke: invokeSyncJSON},
	}
}

func fileJSONContractCases() map[string]jsonContractCase {
	return map[string]jsonContractCase{
		"onedrive-go ls": {contract: jsonDocument, schema: schemaOf[lsJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, fake := jsonContractFileContext(t)
			fake.addFile("notes.txt", "hello")
			return runDetachedLeaf(t, leaf, cc, "/")
		}},
		"onedrive-go stat": {contract: jsonDocument, schema: schemaOf[statJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, fake := jsonContractFileContext(t)
			fake.addFile("notes.txt", "hello")
			return runDetachedLeaf(t, leaf, cc, "/notes.txt")
		}},
		"onedrive-go get": {
			contract: jsonDocument, schema: schemaOf[getJSONOutput](), otherSchemas: []jsonSchema{schemaOf[folderTransferJSONOutput[getJSONOutput]]()},
			invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
				t.Helper()
				cc, fake := jsonContractFileContext(t)
				fake.addFile("notes.txt", "hello")
				return runDetachedLeaf(t, leaf, cc, "/notes.txt", filepath.Join(t.TempDir(), "notes.txt"))
			},
		},
		"onedrive-go put": {
			contract: jsonDocument, schema: schemaOf[putJSONOutput](), otherSchemas: []jsonSchema{schemaOf[folderTransferJSONOutput[putJSONOutput]]()},
			invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
				t.Helper()
				cc, _ := jsonContractFileContext(t)
				local := filepath.Join(t.TempDir(), "upload.txt")
				// Zero bytes: an empty file is a create and a modification-time
				// update, which the fake drive serves; a file with content goes
				// through an upload session, which it does not. The JSON
				// contract is the same.
				require.NoError(t, os.WriteFile(local, nil, 0o600))
				return runDetachedLeaf(t, leaf, cc, local, "/upload.txt")
			},
		},
		"onedrive-go rm": {contract: jsonDocument, schema: schemaOf[rmJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, fake := jsonContractFileContext(t)
			fake.addFile("old.txt", "bye")
			return runDetachedLeaf(t, leaf, cc, "/old.txt")
		}},
		"onedrive-go mkdir": {contract: jsonDocument, schema: schemaOf[mkdirJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, _ := jsonContractFileContext(t)
			return runDetachedLeaf(t, leaf, cc, "/Reports")
		}},
		"onedrive-go mv": {contract: jsonDocument, schema: schemaOf[mvJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, fake := jsonContractFileContext(t)
			fake.addFile("a.txt", "a")
			return runDetachedLeaf(t, leaf, cc, "/a.txt", "/b.txt")
		}},
		"onedrive-go cp": {contract: jsonDocument, schema: schemaOf[cpJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, fake := jsonContractFileContext(t)
			fake.addFile("a.txt", "a")
			cc.seams.cpAwaitCopy = func(_ context.Context, _ *CLIContext, _ *graph.Client, monitorURL string) (string, error) {
				return strings.TrimPrefix(monitorURL, fakeGraphCopyMonitorPrefix), nil
			}
			return runDetachedLeaf(t, leaf, cc, "/a.txt", "/b.txt")
		}},
	}
}

func recycleBinJSONContractCases() map[string]jsonContractCase {
	return map[string]jsonContractCase{
		"onedrive-go recycle-bin list": {contract: jsonDocument, schema: schemaOf[recycleBinListJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, fake := jsonContractFileContext(t)
			fake.addRecycled("gone.txt")
			return runDetachedLeaf(t, leaf, cc)
		}},
		"onedrive-go recycle-bin restore": {contract: jsonDocument, schema: schemaOf[recycleBinJSONItem](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, fake := jsonContractFileContext(t)
			item := fake.addRecycled("gone.txt")
			return runDetachedLeaf(t, leaf, cc, item.ID)
		}},
		"onedrive-go recycle-bin empty": {contract: jsonDocument, schema: schemaOf[recycleBinEmptyJSONOutput](), invoke: func(t *testing.T, leaf *cobra.Command) (string, error) {
			t.Helper()
			cc, fake := jsonContractFileContext(t)
			fake.addRecycled("gone.txt")
			return runDetachedLeaf(t, leaf, cc, "--confirm")
		}},
	}
}

// invokeLoginJSON covers login from the point the person has signed in. The
// device-code and browser exchanges talk to the Microsoft identity platform
// directly and have no fake; everything login prints happens after them, in
// completeLogin, which this drives through a fake Graph server.
func invokeLoginJSON(t *testing.T, leaf *cobra.Command) (string, error) {
	t.Helper()
	require.Equal(t, "login", leaf.Name())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{"id":"user-123","displayName":"User Example","mail":"user@example.com"}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{"id":"drive-123","name":"OneDrive","driveType":"personal"}`)
		default:
			http.Error(w, "unexpected graph path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	tempPath := pendingTokenPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(tempPath), 0o700))
	require.NoError(t, tokenfile.Save(tempPath, &oauth2.Token{AccessToken: "pending", TokenType: "Bearer"}))

	var stdout bytes.Buffer
	cc := newCommandContext(&bytes.Buffer{}, filepath.Join(t.TempDir(), "config.toml"))
	cc.OutputWriter = &stdout
	cc.Flags.JSON = true
	cc.Runtime = newDiscoverAccountTestRuntime(srv.URL)

	err := completeLogin(t.Context(), cc, staticTokenSource{}, tempPath)

	return stdout.String(), err
}

func invokeDriveSearchJSON(t *testing.T, leaf *cobra.Command) (string, error) {
	t.Helper()

	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.UserID = snapshotTestUserID123
		account.DisplayName = snapshotTestDisplayNameAliceSmith
	})
	writeAccessTokenFile(t, cid, "token-business-search")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{"id":"user-123","displayName":"Alice Smith","mail":"alice@contoso.com"}`)
		case testGraphSitesPath:
			writeTestResponse(t, w, `{"value":[]}`)
		default:
			http.Error(w, "unexpected graph path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	cc := newCommandContext(&bytes.Buffer{}, filepath.Join(t.TempDir(), "missing-config.toml"))
	cc.GraphBaseURL = srv.URL

	return runDetachedLeaf(t, leaf, cc, "marketing")
}

func invokeSyncJSON(t *testing.T, leaf *cobra.Command) (string, error) {
	t.Helper()

	cc, _ := jsonContractConfigContext(t)
	cc.seams.syncRunOnce = func(
		_ context.Context,
		_ *config.Holder,
		drives []*config.ResolvedDrive,
		mode syncengine.SyncMode,
		_ syncengine.RunOptions,
		_ *slog.Logger,
		_ string,
	) multisync.RunOnceResult {
		identity := testStandaloneMountIdentity(drives[0].CanonicalID)
		return multisync.RunOnceResult{
			Startup: multisync.StartupSelectionSummary{
				Results: []multisync.MountStartupResult{{
					Identity: identity, DisplayName: drives[0].DisplayName, Status: multisync.MountStartupRunnable,
				}},
			},
			Reports: []*multisync.MountReport{{
				Identity: identity, DisplayName: drives[0].DisplayName, Report: &syncengine.Report{Mode: mode},
			}},
		}
	}

	return runDetachedLeaf(t, leaf, cc)
}

// drive add without a selector prints only guidance, so --json is refused
// rather than wrapped around prose.
//
// Validates: R-6.6.33
func TestDriveAdd_JSONWithoutSelectorIsAUsageError(t *testing.T) {
	setTestDriveHome(t)

	leaf := findLeaf(t, "onedrive-go drive add")
	leaf.SetContext(t.Context())
	cc := newCommandContext(&bytes.Buffer{}, filepath.Join(t.TempDir(), "config.toml"))

	stdout, err := runDetachedLeaf(t, leaf, cc)
	assertJSONContract(t, jsonRejected, jsonSchema{}, stdout, err)

	// And end to end through main: exit status 2, nothing on stdout.
	var mainStdout, mainStderr bytes.Buffer
	code := mainWithWriters([]string{"--json", "--config", cc.CfgPath, "drive", "add"}, &mainStdout, &mainStderr)
	assert.Equal(t, exitUsage, code)
	assert.Empty(t, mainStdout.String())
	assert.Contains(t, mainStderr.String(), "drive list --json")
}

// ls on a file lists that one file, as ls(1) does, instead of failing with
// OneDrive's "Children cannot be listed from an item that is not a folder".
//
// Validates: R-6.6.26
func TestLs_OnAFileListsThatFile(t *testing.T) {
	setTestDriveHome(t)

	cc, fake := jsonContractFileContext(t)
	fake.addFile("notes.txt", "hello")
	stdout, err := runDetachedLeaf(t, lsLeaf(t), cc, "/notes.txt")
	require.NoError(t, err)

	var items []lsJSONItem
	require.NoError(t, json.Unmarshal([]byte(stdout), &items), stdout)
	require.Len(t, items, 1)
	assert.Equal(t, "notes.txt", items[0].Name)
	assert.False(t, items[0].IsFolder)
	assert.Equal(t, int64(len("hello")), items[0].Size)

	textCC, textFake := jsonContractFileContext(t)
	textFake.addFile("notes.txt", "hello")
	var textOut bytes.Buffer
	textCC.OutputWriter = &textOut
	textCC.StatusWriter = io.Discard
	leaf := findLeaf(t, "onedrive-go ls")
	leaf.SetContext(context.WithValue(t.Context(), cliContextKey{}, textCC))
	leaf.SetArgs([]string{"/notes.txt"})
	leaf.SetOut(io.Discard)
	leaf.SetErr(io.Discard)
	require.NoError(t, leaf.Execute())
	assert.Contains(t, textOut.String(), "notes.txt")
	assert.NotContains(t, textOut.String(), "notes.txt/", "a file is not marked as a folder")

	// A path that does not exist still fails as not found.
	missingCC, _ := jsonContractFileContext(t)
	_, err = runDetachedLeaf(t, lsLeaf(t), missingCC, "/missing")
	require.ErrorIs(t, err, graph.ErrNotFound)
}

func lsLeaf(t *testing.T) *cobra.Command {
	t.Helper()

	leaf := findLeaf(t, "onedrive-go ls")
	leaf.SetContext(t.Context())

	return leaf
}

// A single get prints the file it wrote: its local path, its size, and
// whether its bytes matched the hash OneDrive holds for it, which is false
// for a file OneDrive answers without one.
//
// Validates: R-1.2.4
func TestGet_JSONPrintsTheFileItWrote(t *testing.T) {
	setTestDriveHome(t)
	const content = "hello"
	hash, err := driveops.ComputeQuickXorHashFromReaderContext(t.Context(), strings.NewReader(content))
	require.NoError(t, err)

	for _, tc := range []struct {
		label, hash      string
		wantHashVerified bool
	}{
		{label: "with a hash", hash: hash, wantHashVerified: true},
		{label: "without a hash"},
	} {
		cc, fake := jsonContractFileContext(t)
		id := addFakeItem(fake, fakeGraphRootID, "notes.txt", false, content)
		fake.mu.Lock()
		fake.items[id].QuickXorHash = tc.hash
		fake.mu.Unlock()
		target := filepath.Join(t.TempDir(), "notes.txt")

		run := runFileCommand(t, getCommand, cc, true, "/notes.txt", target)

		require.NoError(t, run.err, run.stderr)
		var doc map[string]any
		require.NoError(t, json.Unmarshal([]byte(run.stdout), &doc), run.stdout)
		assert.Equal(t, map[string]any{
			"path": target, "size": float64(len(content)), "hash_verified": tc.wantHashVerified,
		}, doc, tc.label)
		assertFileHolds(t, target, content)
	}
}

// put prints every remote path in one form, a single file's and a folder's
// files alike: the cleaned path with one leading slash, whatever form it was
// typed in.
//
// Validates: R-1.3.4
func TestPut_JSONPrintsEachPathWithOneLeadingSlash(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	addFakeItem(fake, fakeGraphRootID, "Docs", true, "")
	local := t.TempDir()
	writeEmptyLocalFiles(t, local, "one.txt", "Backup/a.txt")
	file := filepath.Join(local, "one.txt")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{file}, want: "/one.txt"},
		{args: []string{file, "two.txt"}, want: "/two.txt"},
		{args: []string{file, "/three.txt"}, want: "/three.txt"},
		{args: []string{file, "Docs/"}, want: "/Docs/one.txt"},
	} {
		run := runFileCommand(t, putCommand, cc, true, tc.args...)
		require.NoError(t, run.err, run.stderr)
		var doc putJSONOutput
		require.NoError(t, json.Unmarshal([]byte(run.stdout), &doc), run.stdout)
		assert.Equal(t, tc.want, doc.Path, strings.Join(tc.args[1:], " "))
	}

	for _, remote := range []string{"Backup", "/Copy/"} {
		run := runFileCommand(t, putCommand, cc, true, filepath.Join(local, "Backup"), remote)
		require.NoError(t, run.err, run.stderr)
		var doc folderTransferJSONOutput[putJSONOutput]
		require.NoError(t, json.Unmarshal([]byte(run.stdout), &doc), run.stdout)
		require.Len(t, doc.Files, 1)
		assert.Equal(t, "/"+strings.Trim(remote, "/")+"/a.txt", doc.Files[0].Path, remote)
	}
}

// The text line of a single put names the remote path in the form --json
// prints it, the cleaned path with one leading slash, whatever form it was
// typed in.
//
// Validates: R-1.3.4
func TestPut_TextNamesEachPathAsJSONDoes(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	addFakeItem(fake, fakeGraphRootID, "Docs", true, "")
	local := t.TempDir()
	writeEmptyLocalFiles(t, local, "one.txt")
	file := filepath.Join(local, "one.txt")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{file}, want: "/one.txt"},
		{args: []string{file, "two.txt"}, want: "/two.txt"},
		{args: []string{file, "/three.txt"}, want: "/three.txt"},
		{args: []string{file, "Docs/"}, want: "/Docs/one.txt"},
		{args: []string{file, "Docs"}, want: "/Docs/one.txt"},
	} {
		run := runFileCommand(t, putCommand, cc, false, tc.args...)
		require.NoError(t, run.err, run.stderr)
		assert.Equal(t, "Uploaded "+tc.want+" (0 B)\n", run.stderr, strings.Join(tc.args[1:], " "))
	}
}

// sharedFolderFileContext is jsonContractFileContext for a shared folder added
// as a drive of its own: the session's paths start at the folder Shared of the
// fake drive, and every name below it is read by name from there.
func sharedFolderFileContext(t *testing.T) (*CLIContext, *fakeGraphDrive) {
	t.Helper()

	cc, fake := jsonContractFileContext(t)
	shared := addFakeItem(fake, fakeGraphRootID, "Shared", true, "")
	sharedCID := driveid.MustCanonicalID("shared:user@example.com:" + rootTestDrive123 + ":" + shared)
	seedCatalogDrive(t, sharedCID, func(drive *config.CatalogDrive) {
		drive.RemoteDriveID = rootTestDrive123
		drive.OwnerAccountCanonical = jsonContractDrive
	})
	cc.Cfg.CanonicalID = sharedCID
	cc.Cfg.RemoteRootItemID = shared

	return cc, fake
}

// On a shared folder, mkdir and put take a path holding empty names ("a//b")
// as the path without them, as on a drive: mkdir creates each folder and
// confirms the last, and put uploads into the folder the path names and
// confirms the file. Both print the path without the empty names.
//
// Validates: R-1.5.2, R-1.3.6, R-1.3.4
func TestFileCommands_OnASharedFolderAPathWithEmptyNamesIsThePathWithoutThem(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := sharedFolderFileContext(t)

	made := runFileCommand(t, "onedrive-go mkdir", cc, true, "/Reports//2026/")
	require.NoError(t, made.err, made.stderr)
	var folder mkdirJSONOutput
	require.NoError(t, json.Unmarshal([]byte(made.stdout), &folder), made.stdout)
	assert.Equal(t, "/Reports/2026", folder.Created)
	assert.Equal(t, fakeItemID(fake, "Shared/Reports/2026"), folder.ID)

	local := filepath.Join(t.TempDir(), "notes.txt")
	require.NoError(t, os.WriteFile(local, nil, 0o600))
	put := runFileCommand(t, putCommand, cc, true, local, "Reports//2026//notes.txt")
	require.NoError(t, put.err, put.stderr)
	var file putJSONOutput
	require.NoError(t, json.Unmarshal([]byte(put.stdout), &file), put.stdout)
	assert.Equal(t, "/Reports/2026/notes.txt", file.Path)
	assert.NotEmpty(t, file.ID)
	assert.Equal(t, fakeItemID(fake, "Shared/Reports/2026/notes.txt"), file.ID)
}
