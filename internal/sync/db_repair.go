package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type StateDBRepairAction string

const (
	StateDBRepairNoState StateDBRepairAction = "no_state"
	StateDBRepairNoop    StateDBRepairAction = "noop"
	StateDBRepairRepair  StateDBRepairAction = "repair"
	StateDBRepairRebuild StateDBRepairAction = "rebuild"
	StateDBRepairReset   StateDBRepairAction = "reset"
)

type StateDBRepairResult struct {
	Action         StateDBRepairAction
	RepairsApplied int
}

type salvagedState struct{}

func RepairStateDB(ctx context.Context, dbPath string, logger *slog.Logger) (StateDBRepairResult, error) {
	if !pathExists(dbPath) {
		return StateDBRepairResult{Action: StateDBRepairNoState}, nil
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err == nil {
		repairs, repairErr := store.RepairIntegritySafe(ctx)
		if repairErr != nil {
			closeErr := store.Close(context.WithoutCancel(ctx))
			if closeErr != nil {
				return StateDBRepairResult{}, errors.Join(
					fmt.Errorf("repair state DB: %w", repairErr),
					fmt.Errorf("close state DB: %w", closeErr),
				)
			}
			return StateDBRepairResult{}, fmt.Errorf("repair state DB: %w", repairErr)
		}

		report, auditErr := store.AuditIntegrity(ctx)
		closeErr := store.Close(context.WithoutCancel(ctx))
		if auditErr != nil {
			if closeErr != nil {
				return StateDBRepairResult{}, errors.Join(
					fmt.Errorf("audit state DB: %w", auditErr),
					fmt.Errorf("close state DB: %w", closeErr),
				)
			}
			return StateDBRepairResult{}, fmt.Errorf("audit state DB: %w", auditErr)
		}
		if closeErr != nil {
			return StateDBRepairResult{}, fmt.Errorf("close state DB: %w", closeErr)
		}

		if !report.HasFindings() {
			action := StateDBRepairNoop
			if repairs > 0 {
				action = StateDBRepairRepair
			}
			return StateDBRepairResult{
				Action:         action,
				RepairsApplied: repairs,
			}, nil
		}
	}

	_, rebuildErr := rebuildStateDB(ctx, dbPath, logger)
	if rebuildErr == nil {
		return StateDBRepairResult{
			Action: StateDBRepairRebuild,
		}, nil
	}

	if resetErr := resetStateDB(ctx, dbPath, logger); resetErr != nil {
		return StateDBRepairResult{}, errors.Join(
			fmt.Errorf("rebuild state DB: %w", rebuildErr),
			fmt.Errorf("reset state DB: %w", resetErr),
		)
	}

	return StateDBRepairResult{Action: StateDBRepairReset}, nil
}

func rebuildStateDB(ctx context.Context, dbPath string, logger *slog.Logger) (salvagedState, error) {
	salvaged, err := readSalvageableState(dbPath)
	if err != nil {
		return salvagedState{}, err
	}

	tempPath, err := tempStateDBRepairPath(dbPath)
	if err != nil {
		return salvagedState{}, err
	}

	store, err := NewSyncStore(ctx, tempPath, logger)
	if err != nil {
		return salvagedState{}, fmt.Errorf("open rebuilt state DB: %w", err)
	}

	closeCtx := context.WithoutCancel(ctx)
	storeClosed := false
	defer func() {
		if storeClosed {
			return
		}
		if closeErr := store.Close(closeCtx); closeErr != nil {
			logger.Debug("close rebuilt state DB after repair error", "error", closeErr.Error(), "path", tempPath)
		}
	}()

	if err := importSalvagedState(ctx, store, salvaged); err != nil {
		return salvagedState{}, err
	}
	if err := store.Close(closeCtx); err != nil {
		return salvagedState{}, fmt.Errorf("close rebuilt state DB: %w", err)
	}
	storeClosed = true

	if err := replaceStateDBFiles(tempPath, dbPath); err != nil {
		return salvagedState{}, err
	}

	return salvaged, nil
}

func resetStateDB(ctx context.Context, dbPath string, logger *slog.Logger) error {
	if err := removeStateDBFiles(dbPath); err != nil {
		return err
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		return fmt.Errorf("create fresh state DB: %w", err)
	}
	if err := store.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("close fresh state DB: %w", err)
	}

	return nil
}

func readSalvageableState(dbPath string) (salvagedState, error) {
	db, err := openReadOnlySyncStoreDB(dbPath)
	if err != nil {
		return salvagedState{}, fmt.Errorf("open existing state DB for repair: %w", err)
	}
	defer db.Close()

	return salvagedState{}, nil
}

func importSalvagedState(ctx context.Context, store *SyncStore, salvaged salvagedState) error {
	_ = ctx
	_ = store
	_ = salvaged
	return nil
}

func tempStateDBRepairPath(dbPath string) (string, error) {
	tempFile, err := localpath.CreateTemp(filepath.Dir(dbPath), "state-db-repair-*.db")
	if err != nil {
		return "", fmt.Errorf("create temporary state DB: %w", err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		return "", fmt.Errorf("close temporary state DB: %w", err)
	}
	if err := localpath.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove temporary state DB placeholder: %w", err)
	}

	return tempPath, nil
}

func replaceStateDBFiles(tempPath, dbPath string) error {
	if err := removeStateDBFiles(dbPath); err != nil {
		return err
	}
	if err := localpath.Rename(tempPath, dbPath); err != nil {
		return fmt.Errorf("replace state DB: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		tempSidecar := tempPath + suffix
		if !pathExists(tempSidecar) {
			continue
		}
		if err := localpath.Rename(tempSidecar, dbPath+suffix); err != nil {
			return fmt.Errorf("replace state DB sidecar %s: %w", suffix, err)
		}
	}

	return nil
}

func removeStateDBFiles(dbPath string) error {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := localpath.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file %s: %w", candidate, err)
		}
	}

	return nil
}

func pathExists(path string) bool {
	_, err := localpath.Stat(path)
	return err == nil
}
