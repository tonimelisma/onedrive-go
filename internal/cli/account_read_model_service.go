package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/failures"
)

// accountReadModelSnapshot is the shared offline account/auth projection used
// by status, whoami, and drive-management commands. Keeping config loading,
// warning handling, and catalog construction in one place prevents each
// command family from rebuilding its own auth/account semantics.
type accountReadModelSnapshot struct {
	Config  *config.Config
	Catalog []accountCatalogEntry
}

type accountReadModelService struct {
	cc *CLIContext
}

func newAccountReadModelService(cc *CLIContext) *accountReadModelService {
	return &accountReadModelService{cc: cc}
}

func (s *accountReadModelService) loadLenientCatalog(ctx context.Context) (accountReadModelSnapshot, error) {
	logger := s.cc.Logger

	cfg, warnings, err := config.LoadOrDefaultLenient(s.cc.CfgPath, logger)
	outcome := config.ClassifyLoadOutcome(err, warnings)
	if err != nil {
		return accountReadModelSnapshot{}, fmt.Errorf("loading config: %w", err)
	}

	if outcome.Class == failures.ClassActionable {
		config.LogWarnings(warnings, logger)
	}

	return accountReadModelSnapshot{
		Config:  cfg,
		Catalog: buildAccountCatalog(ctx, cfg, logger),
	}, nil
}

func (s *accountReadModelService) statusAccounts(snapshot accountReadModelSnapshot) []statusAccount {
	return buildStatusAccountsFromCatalog(snapshot.Config, snapshot.Catalog, &liveSyncStateQuerier{logger: s.cc.Logger})
}

func (s *accountReadModelService) configuredAuthRequirements(
	snapshot accountReadModelSnapshot,
	include func(accountCatalogEntry) bool,
) []accountAuthRequirement {
	return catalogAuthRequirements(snapshot.Catalog, include)
}

func (s *accountReadModelService) whoamiAuthRequired(
	snapshot accountReadModelSnapshot,
	authenticatedEmail string,
) []accountAuthRequirement {
	return whoamiAuthRequiredAccounts(snapshot.Catalog, authenticatedEmail)
}
