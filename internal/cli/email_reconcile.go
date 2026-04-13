package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func (cc *CLIContext) probeAccountIdentity(
	ctx context.Context,
	accountCID driveid.CanonicalID,
	scope string,
) (config.EmailReconcileResult, error) {
	var zero config.EmailReconcileResult

	profile, found, err := config.LookupAccountProfile(accountCID)
	if err != nil {
		return zero, fmt.Errorf("lookup account profile: %w", err)
	}
	if !found || profile.UserID == "" {
		return zero, nil
	}

	tokenPath := config.DriveTokenPath(accountCID)
	if tokenPath == "" {
		return zero, fmt.Errorf("cannot determine token path for account %q", accountCID)
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, cc.Logger)
	if err != nil {
		return zero, fmt.Errorf("load token source for %s: %w", accountCID, err)
	}

	client, err := newGraphClientWithHTTP(
		cc.graphBaseURL(),
		cc.runtime().BootstrapMeta(),
		ts,
		cc.Logger,
	)
	if err != nil {
		return zero, fmt.Errorf("create graph client for %s: %w", accountCID, err)
	}

	attachAccountAuthProof(client, newAuthProofRecorder(cc.Logger), accountCID.Email(), scope)

	user, err := client.Me(ctx)
	if err != nil {
		return zero, fmt.Errorf("fetch authenticated user for %s: %w", accountCID, err)
	}

	result, err := cc.reconcileGraphUser(accountCID, user)
	if err != nil {
		return zero, err
	}

	return result, nil
}

func (cc *CLIContext) reconcileGraphUser(
	accountCID driveid.CanonicalID,
	user *graph.User,
) (config.EmailReconcileResult, error) {
	if user == nil {
		return config.EmailReconcileResult{}, nil
	}

	result, err := config.ReconcileAccountEmail(cc.CfgPath, accountCID, user.ID, user.Email, cc.Logger)
	if err != nil {
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

	for i := range cc.Flags.Drive {
		for j := range result.DriveRenames {
			rename := result.DriveRenames[j]
			if cc.Flags.Drive[i] == rename.From.String() {
				cc.Flags.Drive[i] = rename.To.String()
			}
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

func (cc *CLIContext) graphBaseURL() string {
	if cc == nil {
		return ""
	}

	if cc.GraphBaseURL != "" {
		return cc.GraphBaseURL
	}

	if cc.Runtime != nil && cc.Runtime.GraphBaseURL != "" {
		return cc.Runtime.GraphBaseURL
	}

	return ""
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
