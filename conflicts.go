package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

// conflictIDPrefixLen is the number of characters to show for the conflict ID
// in table output. 8 chars is sufficient for uniqueness in typical use.
const conflictIDPrefixLen = 8

func newConflictsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conflicts",
		Short: "List unresolved sync conflicts",
		Long: `Display sync conflicts from the state database.

By default shows only unresolved conflicts. Use --history to include
resolved conflicts. Use 'onedrive-go resolve' to resolve conflicts.`,
		RunE: runConflicts,
	}

	cmd.Flags().Bool("history", false, "show all conflicts including resolved ones")

	return cmd
}

// conflictJSON is the JSON-serializable representation of a conflict.
type conflictJSON struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	ConflictType string `json:"conflict_type"`
	DetectedAt   string `json:"detected_at"`
	LocalHash    string `json:"local_hash,omitempty"`
	RemoteHash   string `json:"remote_hash,omitempty"`
	Resolution   string `json:"resolution"`
	ResolvedAt   string `json:"resolved_at,omitempty"`
	ResolvedBy   string `json:"resolved_by,omitempty"`
}

func runConflicts(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())

	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	mgr, err := sync.NewBaselineManager(dbPath, cc.Logger)
	if err != nil {
		return err
	}
	defer mgr.Close()

	ctx := cmd.Context()

	history, err := cmd.Flags().GetBool("history")
	if err != nil {
		return err
	}

	var conflicts []sync.ConflictRecord
	if history {
		conflicts, err = mgr.ListAllConflicts(ctx)
	} else {
		conflicts, err = mgr.ListConflicts(ctx)
	}

	if err != nil {
		return err
	}

	if len(conflicts) == 0 {
		if history {
			fmt.Println("No conflicts in history.")
		} else {
			fmt.Println("No unresolved conflicts.")
		}

		return nil
	}

	if flagJSON {
		return printConflictsJSON(conflicts)
	}

	printConflictsTable(conflicts, history)

	return nil
}

func printConflictsJSON(conflicts []sync.ConflictRecord) error {
	items := make([]conflictJSON, len(conflicts))
	for i := range conflicts {
		c := &conflicts[i]
		cj := conflictJSON{
			ID:           c.ID,
			Path:         c.Path,
			ConflictType: c.ConflictType,
			DetectedAt:   time.Unix(0, c.DetectedAt).UTC().Format(time.RFC3339),
			LocalHash:    c.LocalHash,
			RemoteHash:   c.RemoteHash,
			Resolution:   c.Resolution,
			ResolvedBy:   c.ResolvedBy,
		}

		if c.ResolvedAt != 0 {
			cj.ResolvedAt = time.Unix(0, c.ResolvedAt).UTC().Format(time.RFC3339)
		}

		items[i] = cj
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(items); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printConflictsTable(conflicts []sync.ConflictRecord, history bool) {
	var headers []string
	if history {
		headers = []string{"ID", "PATH", "TYPE", "RESOLUTION", "RESOLVED BY", "DETECTED"}
	} else {
		headers = []string{"ID", "PATH", "TYPE", "DETECTED"}
	}

	rows := make([][]string, len(conflicts))

	for i := range conflicts {
		c := &conflicts[i]
		idPrefix := c.ID
		if len(idPrefix) > conflictIDPrefixLen {
			idPrefix = idPrefix[:conflictIDPrefixLen]
		}

		detected := time.Unix(0, c.DetectedAt).UTC().Format(time.RFC3339)

		if history {
			rows[i] = []string{idPrefix, c.Path, c.ConflictType, c.Resolution, c.ResolvedBy, detected}
		} else {
			rows[i] = []string{idPrefix, c.Path, c.ConflictType, detected}
		}
	}

	printTable(os.Stdout, headers, rows)
}
