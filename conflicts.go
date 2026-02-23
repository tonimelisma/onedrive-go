package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// conflictIDPrefixLen is the number of characters to show for the conflict ID
// in table output. 8 chars is sufficient for uniqueness in typical use.
const conflictIDPrefixLen = 8

func newConflictsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "conflicts",
		Short: "List unresolved sync conflicts",
		Long: `Display all unresolved sync conflicts from the state database.

Shows conflicts detected during sync that require user resolution.
Use 'onedrive-go resolve' to resolve conflicts.`,
		RunE: runConflicts,
	}
}

// conflictJSON is the JSON-serializable representation of a conflict.
type conflictJSON struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	ConflictType string `json:"conflict_type"`
	DetectedAt   string `json:"detected_at"`
	LocalHash    string `json:"local_hash,omitempty"`
	RemoteHash   string `json:"remote_hash,omitempty"`
}

func runConflicts(cmd *cobra.Command, _ []string) error {
	logger := buildLogger()

	dbPath := config.DriveStatePath(resolvedCfg.CanonicalID)
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", resolvedCfg.CanonicalID)
	}

	mgr, err := sync.NewBaselineManager(dbPath, logger)
	if err != nil {
		return err
	}
	defer mgr.Close()

	ctx := cmd.Context()

	conflicts, err := mgr.ListConflicts(ctx)
	if err != nil {
		return err
	}

	if len(conflicts) == 0 {
		fmt.Println("No unresolved conflicts.")
		return nil
	}

	if flagJSON {
		return printConflictsJSON(conflicts)
	}

	printConflictsTable(conflicts)

	return nil
}

func printConflictsJSON(conflicts []sync.ConflictRecord) error {
	items := make([]conflictJSON, len(conflicts))
	for i := range conflicts {
		c := &conflicts[i]
		items[i] = conflictJSON{
			ID:           c.ID,
			Path:         c.Path,
			ConflictType: c.ConflictType,
			DetectedAt:   time.Unix(0, c.DetectedAt).UTC().Format(time.RFC3339),
			LocalHash:    c.LocalHash,
			RemoteHash:   c.RemoteHash,
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(items); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printConflictsTable(conflicts []sync.ConflictRecord) {
	headers := []string{"ID", "PATH", "TYPE", "DETECTED"}
	rows := make([][]string, len(conflicts))

	for i := range conflicts {
		c := &conflicts[i]
		idPrefix := c.ID
		if len(idPrefix) > conflictIDPrefixLen {
			idPrefix = idPrefix[:conflictIDPrefixLen]
		}

		detected := time.Unix(0, c.DetectedAt).UTC().Format(time.RFC3339)

		rows[i] = []string{idPrefix, c.Path, c.ConflictType, detected}
	}

	printTable(os.Stdout, headers, rows)
}
