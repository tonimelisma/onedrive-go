package e2e

import (
	"encoding/json"
	"fmt"
	"testing"

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
	MountRef      string               `json:"mount_ref"`
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
	Perf                  *statusPerfJSON   `json:"perf,omitempty"`
	PerfUnavailableReason string            `json:"perf_unavailable_reason,omitempty"`
}

// statusPerfJSON is the part of a mount's live perf snapshot in
// `status --perf --json` that the suite reads.
type statusPerfJSON struct {
	HTTPRequestCount int `json:"http_request_count"`
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
