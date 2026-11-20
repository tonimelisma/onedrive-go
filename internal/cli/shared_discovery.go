package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

type sharedDiscoveryTarget struct {
	Selector            string
	IsFolder            bool
	Name                string
	AccountEmail        string
	SharedByName        string
	SharedByEmail       string
	OwnerIdentityStatus sharedOwnerIdentityStatus
	ModifiedAt          string
	Size                int64
	RemoteDriveID       string
	RemoteItemID        string
}

type sharedOwnerIdentityStatus string

const (
	sharedOwnerIdentityStatusAvailable            sharedOwnerIdentityStatus = "available"
	sharedOwnerIdentityStatusUnavailableRetryable sharedOwnerIdentityStatus = "unavailable_retryable"
	sharedOwnerUnavailableRetryLaterText                                    = "owner unavailable from Microsoft Graph; try again later"
)

type sharedDiscoveryResult struct {
	Targets               []sharedDiscoveryTarget
	AccountsRequiringAuth []accountAuthRequirement
	AccountsDegraded      []accountDegradedNotice
}

func discoverSharedTargets(
	ctx context.Context,
	cc *CLIContext,
	runtime *driveops.SessionRuntime,
	accounts []accountView,
) sharedDiscoveryResult {
	result := sharedDiscoveryResult{}
	seen := make(map[string]struct{})

	// Each account is searched at the same time (runBounded); its results
	// are kept at its index and merged in account order below.
	perAccount := make([]sharedDiscoveryResult, len(accounts))
	runBounded(ctx, cc.Logger, len(accounts),
		func(i int) string { return "while searching the items shared with " + accounts[i].Email },
		func(i int) {
			entry := &accounts[i]
			if entry.AuthHealth.State == authStateAuthenticationNeeded {
				perAccount[i].AccountsRequiringAuth = []accountAuthRequirement{authRequirement(
					entry.Email,
					entry.DisplayName,
					entry.DriveType,
					entry.StateDBCount,
					entry.AuthHealth,
				)}
				return
			}

			targets, authRequired, degraded := discoverSharedTargetsForAccount(ctx, cc, runtime, entry)
			perAccount[i] = sharedDiscoveryResult{
				Targets:               targets,
				AccountsRequiringAuth: authRequired,
				AccountsDegraded:      degraded,
			}
		},
		// A panic is this account's failed search, reported the way a
		// failed search is, while the other accounts are searched.
		func(i int) {
			entry := &accounts[i]
			perAccount[i] = sharedDiscoveryResult{AccountsDegraded: []accountDegradedNotice{
				sharedDiscoveryDegradedNotice(entry.Email, entry.DisplayName, entry.DriveType),
			}}
		},
	)

	for i := range perAccount {
		result.AccountsRequiringAuth = append(result.AccountsRequiringAuth, perAccount[i].AccountsRequiringAuth...)
		result.AccountsDegraded = append(result.AccountsDegraded, perAccount[i].AccountsDegraded...)

		accountTargets := perAccount[i].Targets
		for j := range accountTargets {
			key := accountTargets[j].AccountEmail + "\x00" + accountTargets[j].RemoteDriveID + "\x00" + accountTargets[j].RemoteItemID
			if _, ok := seen[key]; ok {
				continue
			}

			seen[key] = struct{}{}
			result.Targets = append(result.Targets, accountTargets[j])
		}
	}

	slices.SortFunc(result.Targets, func(a, b sharedDiscoveryTarget) int {
		if a.IsFolder != b.IsFolder {
			if a.IsFolder {
				return 1
			}
			return -1
		}
		if a.SharedByEmail != b.SharedByEmail {
			return strings.Compare(a.SharedByEmail, b.SharedByEmail)
		}
		if a.Name != b.Name {
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		}
		return strings.Compare(a.Selector, b.Selector)
	})

	result.AccountsRequiringAuth = mergeAuthRequirements(result.AccountsRequiringAuth)
	result.AccountsDegraded = mergeDegradedNotices(result.AccountsDegraded)

	return result
}

func discoverSharedTargetsForAccount(
	ctx context.Context,
	cc *CLIContext,
	runtime *driveops.SessionRuntime,
	entry *accountView,
) ([]sharedDiscoveryTarget, []accountAuthRequirement, []accountDegradedNotice) {
	logger := cc.Logger
	tokenCandidates := sharedDiscoveryTokenCandidates(entry)
	if len(tokenCandidates) == 0 {
		logger.Debug("shared discovery missing account tokens",
			"email", entry.Email,
		)

		return nil, nil, []accountDegradedNotice{
			sharedDiscoveryDegradedNotice(entry.Email, entry.DisplayName, entry.DriveType),
		}
	}

	var authRequired []accountAuthRequirement

	for i := range tokenCandidates {
		tokenCID := tokenCandidates[i]
		tokenPath := config.DriveTokenPath(tokenCID)
		if tokenPath == "" {
			logger.Debug("shared discovery missing token path",
				"account", tokenCID.String(),
			)
			continue
		}

		client, err := runtime.BootstrapClient(tokenPath)
		if err != nil {
			logger.Debug("shared discovery token load failed",
				"account", tokenCID.String(),
				"error", err,
			)
			authRequired = append(authRequired, tokenDiscoveryAuthRequirement(tokenCID, err, logger))
			continue
		}
		attachAccountAuthProof(client, newAuthProofRecorder(logger), tokenCID, "shared-discovery")

		targets, err := searchSharedTargets(ctx, client, tokenCID.Email(), logger)
		if err != nil {
			if errors.Is(err, graph.ErrUnauthorized) {
				authRequired = append(authRequired, tokenAuthRequirement(tokenCID, authReasonSyncAuthRejected, logger))
				continue
			}

			logger.Warn("degrading shared discovery after search failure",
				"account", tokenCID.String(),
				"error", err,
			)
			continue
		}

		return targets, nil, nil
	}

	if len(authRequired) > 0 {
		return nil, mergeAuthRequirements(authRequired), nil
	}

	// No sign-in answered the search: the account's shared items are unknown.
	return nil, nil, []accountDegradedNotice{
		sharedDiscoveryDegradedNotice(entry.Email, entry.DisplayName, entry.DriveType),
	}
}

func sharedDiscoveryTokenCandidates(entry *accountView) []driveid.CanonicalID {
	if entry == nil {
		return nil
	}

	candidates := make([]driveid.CanonicalID, 0, len(entry.TokenDriveIDs)+1)
	appendCandidate := func(cid driveid.CanonicalID) {
		if cid.IsZero() {
			return
		}
		for i := range candidates {
			if candidates[i] == cid {
				return
			}
		}
		candidates = append(candidates, cid)
	}

	appendCandidate(entry.RepresentativeTokenID)
	for i := range entry.TokenDriveIDs {
		appendCandidate(entry.TokenDriveIDs[i])
	}

	return candidates
}

func searchSharedTargets(
	ctx context.Context,
	client *graph.Client,
	accountEmail string,
	logger *slog.Logger,
) ([]sharedDiscoveryTarget, error) {
	items, err := client.SearchDriveItems(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("search shared items: %w", err)
	}

	targets := make([]sharedDiscoveryTarget, 0, len(items))
	ignoredCount := 0

	for i := range items {
		item := items[i]
		if item.RemoteDriveID == "" || item.RemoteItemID == "" {
			ignoredCount++
			continue
		}

		targets = append(targets, sharedDiscoveryTarget{
			Selector: sharedref.Ref{
				AccountEmail:  accountEmail,
				RemoteDriveID: item.RemoteDriveID,
				RemoteItemID:  item.RemoteItemID,
			}.String(),
			IsFolder:            item.IsFolder || item.RemoteIsFolder,
			Name:                item.Name,
			AccountEmail:        accountEmail,
			SharedByName:        item.SharedOwnerName,
			SharedByEmail:       item.SharedOwnerEmail,
			OwnerIdentityStatus: sharedOwnerIdentityStatusForIdentity(item.SharedOwnerName, item.SharedOwnerEmail),
			ModifiedAt:          formatAPITime(item.ModifiedAt),
			Size:                item.Size,
			RemoteDriveID:       item.RemoteDriveID,
			RemoteItemID:        item.RemoteItemID,
		})
	}

	if ignoredCount > 0 {
		logger.Debug("shared discovery ignored search results without actionable remote identity",
			"email", accountEmail,
			"ignored_count", ignoredCount,
			"actionable_count", len(targets),
			"search_count", len(items),
		)
	}

	enrichSharedTargets(ctx, client, targets, logger)

	return targets, nil
}

// discoveryConcurrency is how many lookups a discovery has under way at once:
// the accounts drive list reads, and the shared items whose sharers it looks
// up. Each lookup is Graph requests of its own, so the number of accounts or
// shared items never sets how many requests are in flight.
const discoveryConcurrency = 5

// runBounded calls work for each index below count, at most
// discoveryConcurrency at once, and returns when every call it started has
// returned. Once ctx ends it starts no more; the caller reads the stop from
// ctx. A panic in one call is that item's failure (R-6.3.10): it is logged,
// where(i) completing "Internal error ...", failed(i) records it when failed
// is set, and the other calls go on.
func runBounded(
	ctx context.Context,
	logger *slog.Logger,
	count int,
	where func(i int) string,
	work func(i int),
	failed func(i int),
) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, discoveryConcurrency)

launch:
	for i := range count {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break launch
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer func() {
				if r := recover(); r != nil {
					//nolint:errcheck // failed records the item's failure; the error is only logged.
					panicguard.Recovered(logger, where(i), r)
					if failed != nil {
						failed(i)
					}
				}
			}()

			work(i)
		}()
	}

	wg.Wait()
}

func enrichSharedTargets(
	ctx context.Context,
	client *graph.Client,
	targets []sharedDiscoveryTarget,
	logger *slog.Logger,
) {
	runBounded(ctx, logger, len(targets),
		func(i int) string { return "while looking up who shared " + targets[i].Name },
		func(i int) { enrichSharedTarget(ctx, client, &targets[i], logger) },
		// A panic leaves the item without its sharer's identity, as a failed
		// lookup does: nothing more to record.
		nil,
	)
}

func enrichSharedTarget(
	ctx context.Context,
	client *graph.Client,
	target *sharedDiscoveryTarget,
	logger *slog.Logger,
) {
	if target == nil {
		return
	}
	if target.SharedByName != "" && target.SharedByEmail != "" {
		return
	}

	if target.RemoteDriveID == "" || target.RemoteItemID == "" {
		return
	}

	enriched, err := client.GetItem(ctx, driveid.New(target.RemoteDriveID), target.RemoteItemID)
	if err != nil {
		logger.Debug("could not enrich shared item identity",
			"name", target.Name,
			"error", err,
		)

		return
	}

	if enriched.SharedOwnerName != "" {
		target.SharedByName = enriched.SharedOwnerName
	}

	if enriched.SharedOwnerEmail != "" {
		target.SharedByEmail = enriched.SharedOwnerEmail
	}

	target.OwnerIdentityStatus = sharedOwnerIdentityStatusForIdentity(target.SharedByName, target.SharedByEmail)
}

type sharedFolderInfo struct {
	cid         driveid.CanonicalID
	target      sharedDiscoveryTarget
	displayName string
}

func projectSharedFolders(
	cfg *config.Config,
	targets []sharedDiscoveryTarget,
) []sharedFolderInfo {
	existingNames := collectExistingDisplayNames(cfg)
	folders := make([]sharedFolderInfo, 0, len(targets))

	for i := range targets {
		target := targets[i]
		if !target.IsFolder {
			continue
		}

		cid, err := driveid.ConstructShared(target.AccountEmail, target.RemoteDriveID, target.RemoteItemID)
		if err != nil {
			continue
		}

		if cfg != nil {
			if _, exists := cfg.Drives[cid]; exists {
				continue
			}
		}

		displayName := deriveSharedDisplayName(sharedDisplayInput{
			Name:          target.Name,
			SharedByName:  target.SharedByName,
			SharedByEmail: target.SharedByEmail,
			RemoteDriveID: target.RemoteDriveID,
			RemoteItemID:  target.RemoteItemID,
		}, existingNames)
		existingNames[displayName] = true

		folders = append(folders, sharedFolderInfo{
			cid:         cid,
			target:      target,
			displayName: displayName,
		})
	}

	return folders
}

type sharedDisplayInput struct {
	Name          string
	SharedByName  string
	SharedByEmail string
	RemoteDriveID string
	RemoteItemID  string
}

func deriveSharedDisplayName(item sharedDisplayInput, existingNames map[string]bool) string {
	folderName := strings.TrimSpace(item.Name)
	if folderName == "" {
		folderName = fallbackSharedItemName
	}

	if item.SharedByName == "" && item.SharedByEmail == "" {
		return firstAvailableSharedDisplayName(existingNames,
			sharedRemoteIdentityDisplayName(item.RemoteDriveID, item.RemoteItemID, folderName),
		)
	}

	if item.SharedByName == "" {
		return firstAvailableSharedDisplayName(existingNames,
			fmt.Sprintf("%s (shared by %s)", folderName, item.SharedByEmail),
			sharedRemoteIdentityDisplayName(item.RemoteDriveID, item.RemoteItemID, folderName),
		)
	}

	firstName := extractFirstName(item.SharedByName)
	candidates := []string{
		fmt.Sprintf("%s's %s", firstName, folderName),
		fmt.Sprintf("%s's %s", item.SharedByName, folderName),
	}
	if item.SharedByEmail != "" {
		candidates = append(candidates, fmt.Sprintf("%s's %s (%s)", item.SharedByName, folderName, item.SharedByEmail))
	}
	candidates = append(candidates, sharedRemoteIdentityDisplayName(item.RemoteDriveID, item.RemoteItemID, folderName))

	return firstAvailableSharedDisplayName(existingNames, candidates...)
}

func extractFirstName(fullName string) string {
	if i := strings.Index(fullName, " "); i > 0 {
		return fullName[:i]
	}

	return fullName
}

func firstAvailableSharedDisplayName(existingNames map[string]bool, candidates ...string) string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}

		if existingNames == nil || !existingNames[candidate] {
			return candidate
		}
	}

	return fallbackSharedItemName
}

func sharedRemoteIdentityDisplayName(remoteDriveID, remoteItemID, fallbackName string) string {
	if remoteDriveID == "" || remoteItemID == "" {
		return fallbackName
	}

	return fmt.Sprintf("%s (shared %s:%s)", fallbackName, remoteDriveID, remoteItemID)
}

func sharedOwnerIdentityStatusForIdentity(name, email string) sharedOwnerIdentityStatus {
	if name != "" || email != "" {
		return sharedOwnerIdentityStatusAvailable
	}

	return sharedOwnerIdentityStatusUnavailableRetryable
}
