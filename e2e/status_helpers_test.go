//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type statusJSON struct {
	Accounts []statusAccountJSON `json:"accounts"`
	Summary  statusSummaryJSON   `json:"summary"`
}

type statusSummaryJSON struct {
	TotalDrives        int `json:"total_drives"`
	TotalSharedFolders int `json:"total_shared_folders,omitempty"`
	hasTotalDrives     bool
}

func (summary *statusSummaryJSON) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode status summary: %w", err)
	}
	raw, ok := fields["total_drives"]
	if !ok {
		*summary = statusSummaryJSON{}
		return nil
	}
	if err := json.Unmarshal(raw, &summary.TotalDrives); err != nil {
		return fmt.Errorf("decode status summary total_drives: %w", err)
	}
	summary.hasTotalDrives = true
	if raw, ok := fields["total_shared_folders"]; ok {
		if err := json.Unmarshal(raw, &summary.TotalSharedFolders); err != nil {
			return fmt.Errorf("decode status summary total_shared_folders: %w", err)
		}
	}
	return nil
}

type statusAccountJSON struct {
	Email  string            `json:"email"`
	Drives []statusDriveJSON `json:"drives"`
}

type statusDriveJSON struct {
	Kind          string               `json:"kind"`
	Name          string               `json:"name"`
	Folder        string               `json:"folder"`
	State         string               `json:"state"`
	SharedFolders []statusDriveJSON    `json:"shared_folders,omitempty"`
	SyncState     *statusSyncStateJSON `json:"sync_state,omitempty"`
	Storage       map[string]any       `json:"storage,omitempty"`
}

type statusSyncStateJSON struct {
	FileCount             int               `json:"file_count"`
	IssueCount            int               `json:"issue_count"`
	RemoteDrift           int               `json:"remote_changes"`
	Retrying              int               `json:"retrying"`
	Issues                []statusIssueJSON `json:"issues"`
	ExamplesLimit         int               `json:"examples_limit"`
	Verbose               bool              `json:"verbose"`
	Perf                  map[string]any    `json:"perf,omitempty"`
	PerfUnavailableReason string            `json:"perf_unavailable_reason,omitempty"`
}

type statusIssueJSON struct {
	Type      string   `json:"type"`
	Title     string   `json:"title"`
	Reason    string   `json:"reason"`
	Action    string   `json:"action"`
	ScopeKind string   `json:"scope_kind"`
	Scope     string   `json:"scope"`
	Count     int      `json:"count"`
	Paths     []string `json:"paths"`
}

func runStatusAllowError(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (statusJSON, string, string, error) {
	t.Helper()

	statusArgs := append([]string{"status"}, args...)
	statusArgs = append(statusArgs, "--json")
	stdout, stderr, err := runCLIWithConfigAllowError(t, cfgPath, env, statusArgs...)
	if err != nil {
		return statusJSON{}, stdout, stderr, err
	}

	var output statusJSON
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		return statusJSON{}, stdout, stderr, fmt.Errorf("decode status json: %w", err)
	}
	if err := validateStatusJSONContract(output); err != nil {
		return statusJSON{}, stdout, stderr, err
	}

	return output, stdout, stderr, nil
}

func runStatusAllDrivesAllowError(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	args ...string,
) (statusJSON, string, string, error) {
	t.Helper()

	statusArgs := append([]string{"status"}, args...)
	statusArgs = append(statusArgs, "--json")
	stdout, stderr, err := runCLIWithConfigAllDrivesAllowError(t, cfgPath, env, statusArgs...)
	if err != nil {
		return statusJSON{}, stdout, stderr, err
	}

	var output statusJSON
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		return statusJSON{}, stdout, stderr, fmt.Errorf("decode status json: %w", err)
	}
	if err := validateStatusJSONContract(output); err != nil {
		return statusJSON{}, stdout, stderr, err
	}

	return output, stdout, stderr, nil
}

func validateStatusJSONContract(status statusJSON) error {
	if !status.Summary.hasTotalDrives {
		return fmt.Errorf("status json missing summary.total_drives")
	}
	actualDrives := countStatusDrives(status)
	if status.Summary.TotalDrives != actualDrives {
		return fmt.Errorf(
			"status json total_drives mismatch: summary=%d top_level_drives=%d",
			status.Summary.TotalDrives,
			actualDrives,
		)
	}
	actualSharedFolders := countStatusSharedFolders(status)
	if status.Summary.TotalSharedFolders != actualSharedFolders {
		return fmt.Errorf(
			"status json total_shared_folders mismatch: summary=%d recursive_shared_folders=%d",
			status.Summary.TotalSharedFolders,
			actualSharedFolders,
		)
	}
	return nil
}

func readStatus(t *testing.T, cfgPath string, env map[string]string, args ...string) statusJSON {
	t.Helper()

	output, stdout, stderr, err := runStatusAllowError(t, cfgPath, env, args...)
	require.NoErrorf(t, err, "status command failed\nstdout: %s\nstderr: %s", stdout, stderr)

	return output
}

func readStatusAllDrives(t *testing.T, cfgPath string, env map[string]string, args ...string) statusJSON {
	t.Helper()

	output, stdout, stderr, err := runStatusAllDrivesAllowError(t, cfgPath, env, args...)
	require.NoErrorf(t, err, "status command failed\nstdout: %s\nstderr: %s", stdout, stderr)

	return output
}

func requireStatusDriveByIdentity(
	t *testing.T,
	status statusJSON,
	canonicalID string,
) statusDriveJSON {
	t.Helper()

	drive, ok := findStatusDriveJSON(status, canonicalID)
	if ok {
		return drive
	}

	require.FailNowf(t, "missing status drive", "drive=%s", canonicalID)
	return statusDriveJSON{}
}

func countStatusDrives(status statusJSON) int {
	total := 0
	for i := range status.Accounts {
		total += len(status.Accounts[i].Drives)
	}

	return total
}

func countStatusSharedFolders(status statusJSON) int {
	total := 0
	for i := range status.Accounts {
		for j := range status.Accounts[i].Drives {
			total += countStatusDriveSharedFolders(status.Accounts[i].Drives[j])
		}
	}

	return total
}

func countStatusDriveSharedFolders(drive statusDriveJSON) int {
	total := len(drive.SharedFolders)
	for i := range drive.SharedFolders {
		total += countStatusDriveSharedFolders(drive.SharedFolders[i])
	}

	return total
}

func TestCountStatusSharedFoldersIncludesNestedSharedFolders(t *testing.T) {
	t.Parallel()

	status := statusJSON{
		Accounts: []statusAccountJSON{{
			Drives: []statusDriveJSON{{
				Name: "parent",
				SharedFolders: []statusDriveJSON{
					{Name: "child-a"},
					{
						Name: "child-b",
						SharedFolders: []statusDriveJSON{{
							Name: "grandchild",
						}},
					},
				},
			}},
		}},
	}

	assert.Equal(t, 1, countStatusDrives(status))
	assert.Equal(t, 3, countStatusSharedFolders(status))
}

func TestValidateStatusJSONContractRejectsLegacyMountShape(t *testing.T) {
	t.Parallel()

	var status statusJSON
	require.NoError(t, json.Unmarshal([]byte(`{
		"summary": {"total_mounts": 1},
		"accounts": [{"email": "user@example.com", "mounts": [{"mount_id": "legacy"}]}]
	}`), &status))

	err := validateStatusJSONContract(status)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing summary.total_drives")
}

func findStatusDriveJSON(status statusJSON, identity string) (statusDriveJSON, bool) {
	var candidates []statusDriveJSON
	email := statusIdentityEmail(identity)
	for i := range status.Accounts {
		if email != "" && status.Accounts[i].Email != email {
			continue
		}
		for j := range status.Accounts[i].Drives {
			candidates = append(candidates, status.Accounts[i].Drives[j])
		}
	}

	if len(candidates) == 1 && !isStatusChildIdentity(identity) {
		return candidates[0], true
	}
	if isStatusChildIdentity(identity) {
		var shared []statusDriveJSON
		for i := range candidates {
			collectStatusSharedFolders(candidates[i], &shared)
		}
		if len(shared) == 1 {
			return shared[0], true
		}
	}

	return statusDriveJSON{}, false
}

func findStatusSharedFolderJSON(drive statusDriveJSON, identity string) (statusDriveJSON, bool) {
	if !isStatusChildIdentity(identity) {
		return drive, true
	}
	var shared []statusDriveJSON
	collectStatusSharedFolders(drive, &shared)
	if len(shared) == 1 {
		return shared[0], true
	}
	return statusDriveJSON{}, false
}

func collectStatusSharedFolders(drive statusDriveJSON, out *[]statusDriveJSON) {
	for i := range drive.SharedFolders {
		*out = append(*out, drive.SharedFolders[i])
		collectStatusSharedFolders(drive.SharedFolders[i], out)
	}
}

func statusIdentityEmail(identity string) string {
	parts := strings.Split(identity, ":")
	switch {
	case len(parts) >= 2 && (parts[0] == "personal" || parts[0] == "business" || parts[0] == "shared"):
		return parts[1]
	default:
		return ""
	}
}

func isStatusChildIdentity(identity string) bool {
	return strings.Contains(identity, "|binding:")
}

func requireStatusDrive(
	t *testing.T,
	status statusJSON,
	canonicalID string,
) statusDriveJSON {
	t.Helper()

	return requireStatusDriveByIdentity(t, status, canonicalID)
}

func readStatusSyncState(t *testing.T, cfgPath string, env map[string]string, args ...string) statusSyncStateJSON {
	t.Helper()

	return readStatusSyncStateForDrive(t, cfgPath, env, resolveDriveSelection(env, ""), args...)
}

func readStatusSyncStateForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	canonicalID string,
	args ...string,
) statusSyncStateJSON {
	t.Helper()

	status := readStatus(t, cfgPath, env, args...)
	driveStatus := requireStatusDriveByIdentity(t, status, canonicalID)
	require.NotNil(t, driveStatus.SyncState, "expected sync_state for %s", canonicalID)
	return *driveStatus.SyncState
}

func pollStatusSyncState(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	timeout time.Duration,
	ready func(statusSyncStateJSON) bool,
	args ...string,
) statusSyncStateJSON {
	return pollStatusSyncStateForDrive(t, cfgPath, env, resolveDriveSelection(env, ""), timeout, ready, args...)
}

func pollStatusSyncStateForDrive(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	canonicalID string,
	timeout time.Duration,
	ready func(statusSyncStateJSON) bool,
	args ...string,
) statusSyncStateJSON {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastStatus statusSyncStateJSON
	var lastStdout string
	var lastStderr string
	var lastErr error

	for attempt := 0; ; attempt++ {
		status, stdout, stderr, err := runStatusAllowError(t, cfgPath, env, args...)
		lastStdout = stdout
		lastStderr = stderr
		lastErr = err
		if err == nil {
			driveStatus := requireStatusDriveByIdentity(t, status, canonicalID)
			if driveStatus.SyncState != nil {
				lastStatus = *driveStatus.SyncState
				if ready(lastStatus) {
					return lastStatus
				}
			}
		}

		if time.Now().After(deadline) {
			require.Failf(
				t,
				"pollStatusSyncState: timed out",
				"after %v waiting for status predicate with args %v\nlast error: %v\nlast status: %+v\nlast stdout: %s\nlast stderr: %s",
				timeout,
				args,
				lastErr,
				lastStatus,
				lastStdout,
				lastStderr,
			)
		}

		sleepForLiveTestPropagation(pollBackoff(attempt))
	}
}
