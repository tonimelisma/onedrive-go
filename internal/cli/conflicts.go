package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// conflictIDPrefixLen is the number of characters to show for the conflict ID
// in table output. 8 hex chars = 32 bits of entropy = 4 billion values,
// sufficient for uniqueness in any realistic conflict set.
const conflictIDPrefixLen = 8

// Resolution strategy aliases (re-export from synctypes package for CLI use).
const (
	resolutionKeepLocal  = synctypes.ResolutionKeepLocal
	resolutionKeepRemote = synctypes.ResolutionKeepRemote
	resolutionKeepBoth   = synctypes.ResolutionKeepBoth
)

func resolveEachConflict(
	cc *CLIContext,
	conflicts []synctypes.ConflictRecord,
	resolution string,
	dryRun bool,
	resolveFn func(id, resolution string) (string, error),
) error {
	if len(conflicts) == 0 {
		cc.Statusf("No unresolved conflicts.\n")
		return nil
	}

	for i := range conflicts {
		c := &conflicts[i]
		if dryRun {
			cc.Statusf("Would resolve %s (%s) as %s\n", c.Path, truncateID(c.ID), resolution)
			continue
		}

		status, err := resolveFn(c.ID, resolution)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", c.Path, err)
		}

		writeConflictResolutionStatus(cc, c.Path, resolution, status)
	}

	return nil
}

func resolveSingleConflict(
	cc *CLIContext,
	idOrPath string,
	resolution string,
	dryRun bool,
	listFn func() ([]synctypes.ConflictRecord, error),
	listAllFn func() ([]synctypes.ConflictRecord, error),
	resolveFn func(id, resolution string) (string, error),
) error {
	conflicts, err := listFn()
	if err != nil {
		return err
	}

	target, found, findErr := findConflict(conflicts, idOrPath)
	if findErr != nil {
		return findErr
	}

	if !found {
		if listAllFn != nil {
			allConflicts, listAllErr := listAllFn()
			if listAllErr != nil {
				return listAllErr
			}

			if resolvedConflict, resolved, findErr := findConflict(allConflicts, idOrPath); findErr != nil {
				return findErr
			} else if resolved && resolvedConflict.Resolution != synctypes.ResolutionUnresolved {
				cc.Statusf("Conflict %s already resolved as %s\n", resolvedConflict.Path, resolvedConflict.Resolution)
				return nil
			}
		}

		return fmt.Errorf("conflict not found: %s", idOrPath)
	}

	if dryRun {
		cc.Statusf("Would resolve %s (%s) as %s\n", target.Path, truncateID(target.ID), resolution)
		return nil
	}

	status, err := resolveFn(target.ID, resolution)
	if err != nil {
		return err
	}

	writeConflictResolutionStatus(cc, target.Path, resolution, status)

	return nil
}

func writeConflictResolutionStatus(cc *CLIContext, conflictPath, resolution, status string) {
	switch syncstore.ConflictRequestStatus(status) {
	case syncstore.ConflictRequestQueued:
		cc.Statusf("Queued %s as %s (engine will resolve on the next sync pass)\n", conflictPath, resolution)
	case syncstore.ConflictRequestAlreadyQueued:
		cc.Statusf("Resolution already queued for %s as %s\n", conflictPath, resolution)
	case syncstore.ConflictRequestAlreadyApplying:
		cc.Statusf("Resolution already applying for %s\n", conflictPath)
	case syncstore.ConflictRequestAlreadyResolved:
		cc.Statusf("Conflict %s is already resolved\n", conflictPath)
	default:
		cc.Statusf("Resolution request for %s returned status %s\n", conflictPath, status)
	}
}

func errAmbiguousPrefix(prefix string) error {
	return fmt.Errorf("ambiguous conflict ID prefix %q — provide more characters", prefix)
}

func findConflict(conflicts []synctypes.ConflictRecord, idOrPath string) (*synctypes.ConflictRecord, bool, error) {
	if idOrPath == "" {
		return nil, false, nil
	}

	for i := range conflicts {
		c := &conflicts[i]
		if c.ID == idOrPath || c.Path == idOrPath {
			return c, true, nil
		}
	}

	var match *synctypes.ConflictRecord
	for i := range conflicts {
		c := &conflicts[i]
		if len(c.ID) >= len(idOrPath) && c.ID[:len(idOrPath)] == idOrPath {
			if match != nil {
				return nil, false, errAmbiguousPrefix(idOrPath)
			}
			match = c
		}
	}

	return match, match != nil, nil
}

type conflictJSON struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	ConflictType string `json:"conflict_type"`
	DetectedAt   string `json:"detected_at"`
	LocalHash    string `json:"local_hash,omitempty"`
	RemoteHash   string `json:"remote_hash,omitempty"`
	State        string `json:"state"`
	Resolution   string `json:"resolution"`
	ResolvedAt   string `json:"resolved_at,omitempty"`
	ResolvedBy   string `json:"resolved_by,omitempty"`
}

func toConflictJSON(c *synctypes.ConflictRecord) conflictJSON {
	return conflictJSON{
		ID:           c.ID,
		Path:         c.Path,
		ConflictType: c.ConflictType,
		DetectedAt:   formatNanoTimestamp(c.DetectedAt),
		LocalHash:    c.LocalHash,
		RemoteHash:   c.RemoteHash,
		State:        conflictDisplayState(c),
		Resolution:   c.Resolution,
		ResolvedBy:   c.ResolvedBy,
		ResolvedAt:   formatNanoTimestamp(c.ResolvedAt),
	}
}

func conflictDisplayState(c *synctypes.ConflictRecord) string {
	if c == nil {
		return ""
	}
	if c.Resolution == synctypes.ResolutionUnresolved {
		return synctypes.ConflictStateUnresolved
	}

	return synctypes.ConflictStateResolved
}

func printConflictsTable(w io.Writer, conflicts []synctypes.ConflictRecord, history bool) error {
	var headers []string
	if history {
		headers = []string{"ID", "PATH", "TYPE", "STATE", "RESOLUTION", "RESOLVED BY", "DETECTED"}
	} else {
		headers = []string{"ID", "PATH", "TYPE", "STATE", "DETECTED"}
	}

	rows := make([][]string, len(conflicts))
	for i := range conflicts {
		c := &conflicts[i]
		idPrefix := truncateID(c.ID)
		detected := formatNanoTimestamp(c.DetectedAt)
		state := conflictDisplayState(c)

		if history {
			rows[i] = []string{idPrefix, c.Path, c.ConflictType, state, c.Resolution, c.ResolvedBy, detected}
		} else {
			rows[i] = []string{idPrefix, c.Path, c.ConflictType, state, detected}
		}
	}

	return printTable(w, headers, rows)
}

type conflictsOutputJSON struct {
	Conflicts []conflictJSON `json:"conflicts"`
}

func printConflictsJSON(w io.Writer, conflicts []synctypes.ConflictRecord) error {
	out := conflictsOutputJSON{
		Conflicts: make([]conflictJSON, len(conflicts)),
	}

	for i := range conflicts {
		out.Conflicts[i] = toConflictJSON(&conflicts[i])
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}
