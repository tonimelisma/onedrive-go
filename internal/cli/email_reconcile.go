package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// identityCheckBudget bounds a check of who an account is: one account's, or
// the several a command checks at the same time (R-6.8.31). The check only
// refreshes a label, so a OneDrive slower than this costs the refresh, never
// the command.
const identityCheckBudget = 5 * time.Second

// errIdentityCheckBudgetSpent is the cause a check's context ends with when
// identityCheckBudget runs out.
var errIdentityCheckBudgetSpent = errors.New("account identity check budget spent")

// errIdentityCheckPanicked is the failure of an account's check that a panic
// cut short; the log line names the bug.
var errIdentityCheckPanicked = errors.New("an internal error stopped the account identity check")

// identityCheckBudgetContext is ctx under identityCheckBudget, or the test's
// stand-in for it.
func identityCheckBudgetContext(ctx context.Context, cc *CLIContext) (context.Context, context.CancelFunc) {
	return budgetContext(ctx, cc.seams.identityCheckBudgetContext, identityCheckBudget, errIdentityCheckBudgetSpent)
}

// probeAccountIdentity asks OneDrive who the account is, when that check is
// due, and applies an email change it reports (reconcileGraphUser). The check
// refreshes a label and is never a precondition of the command
// (R-6.8.31):
//   - It is asked only when due (config.CatalogAccount.IdentityCheckDue);
//     otherwise it returns at once without a request.
//   - It waits on OneDrive for at most identityCheckBudget, retries included.
//   - A failure other than a sign-in failure is logged at Debug and returns
//     the zero result, and the command goes on with the stored identity.
//
// The errors it returns are: the catalog cannot be read; a sign-in failure
// (isSignInRequired), from loading the saved login or from the request,
// which the drive requests would meet the same way; and an email change
// OneDrive reported that could not be applied. An applied answer records the
// next check 24 hours on, and a failed one 15 minutes on; a check the
// command's own end cut short, a sign-in failure and a change not applied
// record nothing, so the next command asks again. A catalog write that fails
// is logged at Debug and fails nothing; the next command then asks again too.
func (cc *CLIContext) probeAccountIdentity(
	ctx context.Context,
	accountCID driveid.CanonicalID,
	scope string,
) (config.EmailReconcileResult, error) {
	askCtx, cancel := identityCheckBudgetContext(ctx, cc)
	defer cancel()

	user, answered, err := cc.askAccountIdentityIfDue(ctx, askCtx, cc.runtime(), accountCID, scope)
	if err != nil || !answered {
		return config.EmailReconcileResult{}, err
	}

	return cc.applyAccountIdentity(ctx, accountCID, user)
}

// probeAccountIdentities is probeAccountIdentity for several accounts: it
// asks them at the same time under one identityCheckBudget, then applies
// their answers one after another, in order, so two email changes never
// run at once. Each account's result and error are at its index.
func (cc *CLIContext) probeAccountIdentities(
	ctx context.Context,
	accountCIDs []driveid.CanonicalID,
	scope string,
) ([]config.EmailReconcileResult, []error) {
	users := make([]*graph.User, len(accountCIDs))
	answered := make([]bool, len(accountCIDs))
	errs := make([]error, len(accountCIDs))

	// The runtime is built here, on the command goroutine: the checks below
	// run on goroutines of their own and receive it (CLIContext.runtime).
	runtime := cc.runtime()
	askCtx, cancel := identityCheckBudgetContext(ctx, cc)
	runBounded(askCtx, cc.Logger, len(accountCIDs),
		func(i int) string { return "while checking the identity of " + accountCIDs[i].String() },
		func(i int) {
			users[i], answered[i], errs[i] = cc.askAccountIdentityIfDue(ctx, askCtx, runtime, accountCIDs[i], scope)
		},
		func(i int) { errs[i] = errIdentityCheckPanicked },
	)
	cancel()

	results := make([]config.EmailReconcileResult, len(accountCIDs))
	for i := range accountCIDs {
		if errs[i] != nil || !answered[i] {
			continue
		}
		results[i], errs[i] = cc.applyAccountIdentity(ctx, accountCIDs[i], users[i])
	}

	return results, errs
}

// askAccountIdentityIfDue asks OneDrive for the account's user when its
// check is due, waiting for at most askCtx; ctx is the command's own, and
// runtime the command's session runtime.
// answered is false when the check was not due, the catalog holds no user
// for the account, or the request failed for a reason other than sign-in;
// it records a failed request's next check itself.
func (cc *CLIContext) askAccountIdentityIfDue(
	ctx context.Context,
	askCtx context.Context,
	runtime *driveops.SessionRuntime,
	accountCID driveid.CanonicalID,
	scope string,
) (user *graph.User, answered bool, err error) {
	catalog, err := config.LoadCatalog()
	if err != nil {
		return nil, false, fmt.Errorf("load catalog for account identity check: %w", err)
	}
	account, found := catalog.AccountByCanonicalID(accountCID)
	if !found || account.UserID == "" || !account.IdentityCheckDue(cc.now()) {
		return nil, false, nil
	}

	user, err = cc.askAccountIdentity(askCtx, runtime, accountCID, scope)
	switch {
	case err == nil:
		return user, true, nil
	case isSignInRequired(err):
		return nil, false, err
	case ctx.Err() != nil:
		cc.Logger.Debug("account identity check stopped with the command",
			"account", accountCID.String(), "error", err)
		return nil, false, nil //nolint:nilerr // the command's own stop is not the check's failure
	default:
		cc.Logger.Debug("account identity check failed; keeping the stored identity",
			"account", accountCID.String(), "error", err, "cause", context.Cause(askCtx))
		cc.recordIdentityCheck(ctx, accountCID, false)
		return nil, false, nil
	}
}

// askAccountIdentity is the request: the account's user, from its saved
// login in runtime, under askCtx.
func (cc *CLIContext) askAccountIdentity(
	askCtx context.Context,
	runtime *driveops.SessionRuntime,
	accountCID driveid.CanonicalID,
	scope string,
) (*graph.User, error) {
	tokenPath := config.DriveTokenPath(accountCID)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for account %q", accountCID)
	}

	client, err := runtime.BootstrapClient(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("load sign-in for %s: %w", accountCID, err)
	}

	attachAccountAuthProof(client, newAuthProofRecorder(cc.Logger), accountCID, scope)

	user, err := client.Me(askCtx)
	if err != nil {
		return nil, fmt.Errorf("fetch authenticated user for %s: %w", accountCID, err)
	}

	return user, nil
}

// applyAccountIdentity applies what OneDrive answered for the account and
// records the next check 24 hours on,
// under the account's name after any email change. A change it cannot apply
// is returned and records nothing, so the next command asks and applies
// again.
func (cc *CLIContext) applyAccountIdentity(
	ctx context.Context,
	accountCID driveid.CanonicalID,
	user *graph.User,
) (config.EmailReconcileResult, error) {
	result, err := cc.reconcileGraphUser(ctx, accountCID, user)
	if err != nil {
		return config.EmailReconcileResult{}, err
	}
	cc.recordIdentityCheck(ctx, remapCanonicalIDWithResult(accountCID, &result), true)

	return result, nil
}

// recordIdentityCheck records when the account's next identity check is due.
// A write that fails is logged and fails nothing: the next command asks
// again.
func (cc *CLIContext) recordIdentityCheck(ctx context.Context, accountCID driveid.CanonicalID, answered bool) {
	if err := config.RecordAccountIdentityCheck(ctx, accountCID, cc.now(), answered); err != nil {
		cc.Logger.Debug("could not record the account identity check",
			"account", accountCID.String(), "error", err)
	}
}

// now is the command's clock.
func (cc *CLIContext) now() time.Time {
	if cc.seams.now != nil {
		return cc.seams.now()
	}

	return time.Now()
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
	result, err := config.CompleteEmailChange(ctx, cc.stateDBMover(ctx), cc.Logger)
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

	result, err := config.ReconcileAccountEmail(ctx,
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
		switch cc.Flags.Account {
		case rename.From.Email():
			cc.Flags.Account = rename.To.Email()
		case rename.From.String():
			cc.Flags.Account = rename.To.String()
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

	// A shared folder's store the change could not assign stays under the
	// old email; the user is told once which file, not the log.
	for _, path := range result.KeptStateDBs {
		cc.Statusf("Kept %s under the account's old email: two accounts had that email, and nothing records "+
			"which of them the shared folder belongs to. Remove it if neither account syncs that folder any more.\n",
			path)
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
