//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
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
	TotalMounts    int `json:"total_mounts"`
	hasTotalMounts bool
}

func (summary *statusSummaryJSON) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode status summary: %w", err)
	}
	raw, ok := fields["total_mounts"]
	if !ok {
		*summary = statusSummaryJSON{}
		return nil
	}
	if err := json.Unmarshal(raw, &summary.TotalMounts); err != nil {
		return fmt.Errorf("decode status summary total_mounts: %w", err)
	}
	summary.hasTotalMounts = true
	return nil
}

type statusAccountJSON struct {
	Email  string            `json:"email"`
	Mounts []statusMountJSON `json:"mounts"`
}

type statusMountJSON struct {
	CanonicalID string               `json:"canonical_id"`
	MountID     string               `json:"mount_id"`
	ChildMounts []statusMountJSON    `json:"child_mounts,omitempty"`
	SyncState   *statusSyncStateJSON `json:"sync_state,omitempty"`
}

type statusSyncStateJSON struct {
	FileCount             int                   `json:"file_count"`
	ConditionCount        int                   `json:"condition_count"`
	RemoteDrift           int                   `json:"remote_drift"`
	Retrying              int                   `json:"retrying"`
	Conditions            []statusConditionJSON `json:"conditions"`
	ExamplesLimit         int                   `json:"examples_limit"`
	Verbose               bool                  `json:"verbose"`
	Perf                  map[string]any        `json:"perf,omitempty"`
	PerfUnavailableReason string                `json:"perf_unavailable_reason,omitempty"`
}

type statusConditionJSON struct {
	ConditionKey  string   `json:"condition_key"`
	ConditionType string   `json:"condition_type"`
	Title         string   `json:"title"`
	Reason        string   `json:"reason"`
	Action        string   `json:"action"`
	ScopeKind     string   `json:"scope_kind"`
	Scope         string   `json:"scope"`
	Count         int      `json:"count"`
	Paths         []string `json:"paths"`
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
	if !status.Summary.hasTotalMounts {
		return fmt.Errorf("status json missing summary.total_mounts")
	}
	actual := countStatusMounts(status)
	if status.Summary.TotalMounts != actual {
		return fmt.Errorf(
			"status json total_mounts mismatch: summary=%d recursive_mount_rows=%d",
			status.Summary.TotalMounts,
			actual,
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

func requireStatusMount(
	t *testing.T,
	status statusJSON,
	canonicalID string,
) statusMountJSON {
	t.Helper()

	for i := range status.Accounts {
		for j := range status.Accounts[i].Mounts {
			if found, ok := findStatusMountJSON(status.Accounts[i].Mounts[j], canonicalID); ok {
				return found
			}
		}
	}

	require.FailNowf(t, "missing status mount", "canonical_id=%s", canonicalID)
	return statusMountJSON{}
}

func countStatusMounts(status statusJSON) int {
	total := 0
	for i := range status.Accounts {
		for j := range status.Accounts[i].Mounts {
			total += countStatusMount(status.Accounts[i].Mounts[j])
		}
	}

	return total
}

func countStatusMount(mount statusMountJSON) int {
	total := 1
	for i := range mount.ChildMounts {
		total += countStatusMount(mount.ChildMounts[i])
	}

	return total
}

func TestCountStatusMountsIncludesNestedChildMounts(t *testing.T) {
	t.Parallel()

	status := statusJSON{
		Accounts: []statusAccountJSON{{
			Mounts: []statusMountJSON{{
				MountID: "parent",
				ChildMounts: []statusMountJSON{
					{MountID: "child-a"},
					{
						MountID: "child-b",
						ChildMounts: []statusMountJSON{{
							MountID: "grandchild",
						}},
					},
				},
			}},
		}},
	}

	assert.Equal(t, 4, countStatusMounts(status))
}

func TestValidateStatusJSONContractRejectsLegacyDriveShape(t *testing.T) {
	t.Parallel()

	var status statusJSON
	require.NoError(t, json.Unmarshal([]byte(`{
		"summary": {"total_drives": 1},
		"accounts": [{"email": "user@example.com", "drives": [{"mount_id": "legacy"}]}]
	}`), &status))

	err := validateStatusJSONContract(status)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing summary.total_mounts")
}

func findStatusMountJSON(mount statusMountJSON, canonicalID string) (statusMountJSON, bool) {
	if mount.CanonicalID == canonicalID || mount.MountID == canonicalID {
		return mount, true
	}
	for i := range mount.ChildMounts {
		if found, ok := findStatusMountJSON(mount.ChildMounts[i], canonicalID); ok {
			return found, true
		}
	}

	return statusMountJSON{}, false
}

func requireStatusDrive(
	t *testing.T,
	status statusJSON,
	canonicalID string,
) statusMountJSON {
	t.Helper()

	return requireStatusMount(t, status, canonicalID)
}

func readStatusSyncState(t *testing.T, cfgPath string, env map[string]string, args ...string) statusSyncStateJSON {
	t.Helper()

	return readStatusSyncStateForMount(t, cfgPath, env, resolveDriveSelection(env, ""), args...)
}

func readStatusSyncStateForMount(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	canonicalID string,
	args ...string,
) statusSyncStateJSON {
	t.Helper()

	status := readStatus(t, cfgPath, env, args...)
	mountStatus := requireStatusMount(t, status, canonicalID)
	require.NotNil(t, mountStatus.SyncState, "expected sync_state for %s", canonicalID)
	return *mountStatus.SyncState
}

func pollStatusSyncState(
	t *testing.T,
	cfgPath string,
	env map[string]string,
	timeout time.Duration,
	ready func(statusSyncStateJSON) bool,
	args ...string,
) statusSyncStateJSON {
	return pollStatusSyncStateForMount(t, cfgPath, env, resolveDriveSelection(env, ""), timeout, ready, args...)
}

func pollStatusSyncStateForMount(
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
			mountStatus := requireStatusMount(t, status, canonicalID)
			if mountStatus.SyncState != nil {
				lastStatus = *mountStatus.SyncState
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

func assertEmptyStatusSnapshotText(t *testing.T, output string) {
	t.Helper()

	assert.Contains(t, output, "No active conditions.", "status should collapse an unsynced drive to an empty sync snapshot")
	assert.NotContains(t, output, "Last sync:", "status should not reintroduce the removed legacy history block")
}
