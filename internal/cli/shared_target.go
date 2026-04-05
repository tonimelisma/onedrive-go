package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"
)

type sharedTarget struct {
	Ref           sharedref.Ref
	OriginalInput string
}

func (t sharedTarget) Selector() string {
	return t.Ref.String()
}

func resolveSharedTargetBootstrap(
	ctx context.Context,
	cmd *cobra.Command,
	args []string,
	cc *CLIContext,
) (*sharedTarget, bool, error) {
	input, ok := sharedTargetInput(cmd, args)
	if !ok {
		return nil, false, nil
	}

	if strings.HasPrefix(input, sharedref.Prefix) {
		ref, err := sharedref.Parse(input)
		if err != nil {
			return nil, false, fmt.Errorf("parse shared target selector: %w", err)
		}

		if cc.Flags.Account != "" && cc.Flags.Account != ref.AccountEmail {
			return nil, false, fmt.Errorf("--account %q does not match shared target account %q", cc.Flags.Account, ref.AccountEmail)
		}

		return &sharedTarget{
			Ref:           ref,
			OriginalInput: input,
		}, true, nil
	}

	accountEmail, err := resolveRawShareAccountEmail(cc.Flags.Account, cc.Logger)
	if err != nil {
		return nil, false, err
	}

	metaClient, err := cc.sharedBootstrapMetaClient(ctx, accountEmail)
	if err != nil {
		return nil, false, err
	}

	item, err := metaClient.ResolveShareURL(ctx, input)
	if err != nil {
		return nil, false, fmt.Errorf("resolving share URL: %w", err)
	}

	return &sharedTarget{
		Ref: sharedref.Ref{
			AccountEmail:  accountEmail,
			RemoteDriveID: item.RemoteDriveID,
			RemoteItemID:  item.RemoteItemID,
		},
		OriginalInput: input,
	}, true, nil
}

func sharedTargetInput(cmd *cobra.Command, args []string) (string, bool) {
	switch cmd.Name() {
	case "stat":
		if len(args) == 1 && isSharedTargetInput(args[0]) {
			return args[0], true
		}
	case "get":
		if len(args) >= 1 && isSharedTargetInput(args[0]) {
			return args[0], true
		}
	case "put":
		if len(args) >= 2 && isSharedTargetInput(args[1]) {
			return args[1], true
		}
	}

	return "", false
}

func isSharedTargetInput(raw string) bool {
	if strings.HasPrefix(raw, sharedref.Prefix) {
		return true
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}

	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func resolveRawShareAccountEmail(explicit string, logger *slog.Logger) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	tokens := config.DiscoverTokens(logger)
	seen := make(map[string]struct{})
	var emails []string

	for _, tokenID := range tokens {
		if _, ok := seen[tokenID.Email()]; ok {
			continue
		}

		seen[tokenID.Email()] = struct{}{}
		emails = append(emails, tokenID.Email())
	}

	switch len(emails) {
	case 0:
		return "", fmt.Errorf("no authenticated accounts found — run 'onedrive-go login' first")
	case 1:
		return emails[0], nil
	default:
		return "", fmt.Errorf("multiple authenticated accounts found — use --account with the share URL")
	}
}

func (cc *CLIContext) sharedBootstrapMetaClient(
	ctx context.Context,
	accountEmail string,
) (*graph.Client, error) {
	accountCID := findTokenFallback(accountEmail, cc.Logger)
	tokenPath := config.DriveTokenPath(accountCID)
	if tokenPath == "" {
		return nil, fmt.Errorf("cannot determine token path for account %q", accountEmail)
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, cc.Logger)
	if err != nil {
		return nil, fmt.Errorf("load token source: %w", err)
	}

	client, err := newGraphClientWithHTTP(cc.GraphBaseURL, cc.httpProvider().BootstrapMeta(), ts, cc.Logger)
	if err != nil {
		return nil, fmt.Errorf("create bootstrap graph client: %w", err)
	}

	recorder := newAuthProofRecorder(cc.Logger)
	attachAccountAuthProof(client, recorder, accountEmail, "shared-bootstrap")

	return client, nil
}

func (cc *CLIContext) sharedTargetHTTPClients(ref sharedref.Ref) driveops.HTTPClients {
	clients := cc.httpProvider().InteractiveForSharedTarget(ref.AccountEmail, ref.RemoteDriveID, ref.RemoteItemID)

	return driveops.HTTPClients{
		Meta:     clients.Meta,
		Transfer: clients.Transfer,
	}
}

func (cc *CLIContext) sharedTargetClients(ctx context.Context, ref sharedref.Ref) (*driveops.AccountClients, error) {
	accountCID := findTokenFallback(ref.AccountEmail, cc.Logger)
	httpClients := cc.sharedTargetHTTPClients(ref)
	provider := driveops.NewSessionProvider(
		nil,
		driveops.StaticClientResolver(httpClients.Meta, httpClients.Transfer),
		"onedrive-go/"+version,
		cc.Logger,
	)
	if cc.GraphBaseURL != "" {
		provider.GraphBaseURL = cc.GraphBaseURL
	}

	clients, err := provider.ClientsForAccount(ctx, accountCID)
	if err != nil {
		return nil, fmt.Errorf("create account clients: %w", err)
	}

	recorder := newAuthProofRecorder(cc.Logger)
	attachAccountAuthProof(clients.Meta, recorder, ref.AccountEmail, "shared-meta")
	attachAccountAuthProof(clients.Transfer, recorder, ref.AccountEmail, "shared-transfer")

	return clients, nil
}

func (cc *CLIContext) resolveSharedItem(ctx context.Context) (*graph.Item, *driveops.AccountClients, error) {
	if cc.SharedTarget == nil {
		return nil, nil, fmt.Errorf("BUG: shared target not resolved")
	}

	clients, err := cc.sharedTargetClients(ctx, cc.SharedTarget.Ref)
	if err != nil {
		return nil, nil, err
	}

	item, err := clients.Meta.GetItem(ctx, driveid.New(cc.SharedTarget.Ref.RemoteDriveID), cc.SharedTarget.Ref.RemoteItemID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading shared item: %w", err)
	}

	if item.RemoteDriveID == "" {
		item.RemoteDriveID = cc.SharedTarget.Ref.RemoteDriveID
	}
	if item.RemoteItemID == "" {
		item.RemoteItemID = cc.SharedTarget.Ref.RemoteItemID
	}

	return item, clients, nil
}
