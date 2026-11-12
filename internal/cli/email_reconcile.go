package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (cc *CLIContext) probeAccountIdentity(
	ctx context.Context,
	accountCID driveid.CanonicalID,
	scope string,
) (config.EmailReconcileResult, error) {
	var zero config.EmailReconcileResult

	catalog, err := config.LoadCatalog()
	if err != nil {
		return zero, fmt.Errorf("load catalog for account probe: %w", err)
	}
	account, found := catalog.AccountByCanonicalID(accountCID)
	if !found || account.UserID == "" {
		return zero, nil
	}

	tokenPath := config.DriveTokenPath(accountCID)
	if tokenPath == "" {
		return zero, fmt.Errorf("cannot determine token path for account %q", accountCID)
	}

	client, err := cc.runtime().BootstrapClient(ctx, tokenPath)
	if err != nil {
		return zero, fmt.Errorf("load sign-in for %s: %w", accountCID, err)
	}

	attachAccountAuthProof(client, newAuthProofRecorder(cc.Logger), accountCID.Email(), scope)

	user, err := client.Me(ctx)
	if err != nil {
		return zero, fmt.Errorf("fetch authenticated user for %s: %w", accountCID, err)
	}

	result, err := cc.reconcileGraphUser(ctx, accountCID, user)
	if err != nil {
		return zero, err
	}

	return result, nil
}

// stateDBMover is the state DB mover an account email change uses: the
// state DBs move under their store locks (syncengine.MoveStateDBs), which
// internal/config cannot take itself.
func (cc *CLIContext) stateDBMover(ctx context.Context) config.StateDBMover {
	return func(moves []config.StateDBMove, begin func() error) error {
		if err := syncengine.MoveStateDBs(ctx, config.DefaultDataDir(), moves, begin, cc.Logger); err != nil {
			return fmt.Errorf("rename sync state: %w", err)
		}
		return nil
	}
}

// finishInterruptedEmailChange finishes an account email change an earlier
// command was cut short in, before this command reads its config, and remaps
// this invocation's selectors from the old names (applyEmailReconcileResult).
// It fails the command when the change cannot be finished: every name the
// command would resolve may be half renamed.
func (cc *CLIContext) finishInterruptedEmailChange(ctx context.Context) error {
	result, err := config.CompleteEmailChange(cc.stateDBMover(ctx), cc.Logger)
	if err != nil {
		return fmt.Errorf("finish an interrupted account email change: %w", err)
	}
	cc.applyEmailReconcileResult(&result)

	return nil
}

// reconcileGraphUser applies an account email change Graph reports, as one
// journaled change (config.ReconcileAccountEmail). While a sync still has
// one of the account's state DBs open nothing is renamed or recorded, and
// the error says to stop that sync and run the command again.
func (cc *CLIContext) reconcileGraphUser(
	ctx context.Context,
	accountCID driveid.CanonicalID,
	user *graph.User,
) (config.EmailReconcileResult, error) {
	if user == nil {
		return config.EmailReconcileResult{}, nil
	}

	result, err := config.ReconcileAccountEmail(
		cc.CfgPath, accountCID, user.ID, user.Email, cc.stateDBMover(ctx), cc.Logger)
	switch {
	case isSyncInUse(err):
		return config.EmailReconcileResult{}, fmt.Errorf(
			"the account's email changed to %s, but its sync state cannot be renamed while a sync uses it: %w; "+
				"stop that sync, then run the command again", user.Email, err)
	case err != nil:
		return config.EmailReconcileResult{}, fmt.Errorf("reconcile account email: %w", err)
	}

	if result.Changed() {
		cc.applyEmailReconcileResult(&result)
	}

	return result, nil
}

func (cc *CLIContext) applyEmailReconcileResult(result *config.EmailReconcileResult) {
	if result == nil || !result.Changed() {
		return
	}

	cc.reconcileMu.Lock()
	defer cc.reconcileMu.Unlock()

	for i := range result.AccountRenames {
		rename := result.AccountRenames[i]
		if cc.Flags.Account == rename.From.Email() {
			cc.Flags.Account = rename.To.Email()
		}

		if cc.SharedTarget != nil && cc.SharedTarget.Ref.AccountEmail == rename.From.Email() {
			cc.SharedTarget.Ref.AccountEmail = rename.To.Email()
		}

		if cc.reconcileNotices == nil {
			cc.reconcileNotices = make(map[string]struct{})
		}

		noticeKey := rename.From.String() + "->" + rename.To.String()
		if _, seen := cc.reconcileNotices[noticeKey]; !seen {
			cc.Statusf(
				"Account email changed from %s to %s; local config and data were updated automatically.\n",
				rename.From.Email(),
				rename.To.Email(),
			)
			cc.reconcileNotices[noticeKey] = struct{}{}
		}
	}

	// The drive selector from ONEDRIVE_GO_DRIVE resolves exactly as --drive
	// does, so an exact old ID there is remapped too.
	for j := range result.DriveRenames {
		rename := result.DriveRenames[j]
		for i := range cc.Flags.Drive {
			if cc.Flags.Drive[i] == rename.From.String() {
				cc.Flags.Drive[i] = rename.To.String()
			}
		}
		if cc.Env.Drive == rename.From.String() {
			cc.Env.Drive = rename.To.String()
		}
	}
}

func (cc *CLIContext) reloadResolvedDriveFromFlags() error {
	if cc == nil {
		return fmt.Errorf("BUG: CLIContext is nil")
	}

	driveSelector, err := cc.Flags.SingleDrive()
	if err != nil {
		return err
	}

	resolved, rawCfg, err := config.ResolveDrive(
		cc.Env,
		config.CLIOverrides{
			ConfigPath: cc.Flags.ConfigPath,
			Drive:      driveSelector,
		},
		cc.Logger,
	)
	if err != nil {
		return fmt.Errorf("resolve updated drive config: %w", err)
	}

	cc.Cfg = resolved
	if cc.Runtime != nil {
		cc.Runtime.UpdateConfig(rawCfg)
	}

	return nil
}

func remapCanonicalIDWithResult(
	current driveid.CanonicalID,
	result *config.EmailReconcileResult,
) driveid.CanonicalID {
	if updated, ok := result.RemapCanonicalID(current); ok {
		return updated
	}

	return current
}

func accountIDsFromResolvedDrives(
	drives []*config.ResolvedDrive,
) ([]driveid.CanonicalID, error) {
	seen := make(map[string]struct{})
	var accounts []driveid.CanonicalID

	for _, rd := range drives {
		accountCID, err := config.TokenAccountCanonicalID(rd.CanonicalID)
		if err != nil {
			return nil, fmt.Errorf("resolve account for %s: %w", rd.CanonicalID, err)
		}
		if accountCID.IsZero() {
			continue
		}

		if _, exists := seen[accountCID.String()]; exists {
			continue
		}

		seen[accountCID.String()] = struct{}{}
		accounts = append(accounts, accountCID)
	}

	return accounts, nil
}
