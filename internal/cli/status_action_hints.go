package cli

import (
	"fmt"
	"strings"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type statusActionHintSet struct {
	values []string
	seen   map[string]struct{}
}

func newStatusActionHintSet() *statusActionHintSet {
	return &statusActionHintSet{
		values: make([]string, 0),
		seen:   make(map[string]struct{}),
	}
}

func (s *statusActionHintSet) add(hint string) {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return
	}
	if _, exists := s.seen[hint]; exists {
		return
	}

	s.seen[hint] = struct{}{}
	s.values = append(s.values, hint)
}

func (s *statusActionHintSet) slice() []string {
	if len(s.values) == 0 {
		return nil
	}

	out := make([]string, len(s.values))
	copy(out, s.values)

	return out
}

func heldDeleteActionHint(canonicalID string) string {
	return fmt.Sprintf("run `onedrive-go --drive %s resolve deletes`.", canonicalID)
}

func deleteSafetyActionHint(canonicalID string, state string) string {
	switch state {
	case stateHeldDeleteHeld:
		return heldDeleteActionHint(canonicalID)
	case stateHeldDeleteApproved:
		return approvedDeleteActionHint(canonicalID)
	default:
		return ""
	}
}

func approvedDeleteActionHint(canonicalID string) string {
	return fmt.Sprintf(
		"run `onedrive-go --drive %s sync` or start `onedrive-go --drive %s sync --watch` to execute approved deletes.",
		canonicalID,
		canonicalID,
	)
}

func queuedConflictActionHint(canonicalID string) string {
	return fmt.Sprintf(
		"run `onedrive-go --drive %s sync` or start `onedrive-go --drive %s sync --watch` to apply queued resolutions.",
		canonicalID,
		canonicalID,
	)
}

func applyingConflictActionHint(canonicalID string) string {
	return fmt.Sprintf(
		"wait for the active sync owner to finish, then run `onedrive-go --drive %s status` again if needed.",
		canonicalID,
	)
}

func unresolvedConflictActionHint(canonicalID string, path string, conflictID string) string {
	return fmt.Sprintf(
		"run `onedrive-go --drive %s resolve local %s` or replace `local` with `remote` or `both`.",
		canonicalID,
		shellQuoteStatusTarget(statusConflictTarget(path, conflictID)),
	)
}

func statusConflictActionHint(canonicalID string, conflict *statusConflictJSON) string {
	if conflict == nil {
		return ""
	}

	switch conflict.State {
	case syncengine.ConflictStateUnresolved:
		return unresolvedConflictActionHint(canonicalID, conflict.Path, conflict.ID)
	case syncengine.ConflictStateQueued:
		return queuedConflictActionHint(canonicalID)
	case syncengine.ConflictStateApplying:
		return applyingConflictActionHint(canonicalID)
	default:
		return ""
	}
}

func statusConflictTarget(path string, conflictID string) string {
	if strings.TrimSpace(path) != "" {
		return path
	}

	return conflictID
}

func shellQuoteStatusTarget(target string) string {
	if target == "" {
		return "''"
	}

	return "'" + strings.ReplaceAll(target, "'", `'"'"'`) + "'"
}
