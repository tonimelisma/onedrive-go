package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

func runSharedGet(cmd *cobra.Command, args []string, cc *CLIContext) error {
	ctx := cmd.Context()

	item, clients, err := cc.resolveSharedItem(ctx)
	if err != nil {
		return err
	}

	if item.IsFolder {
		localPath := item.Name
		if len(args) > 1 {
			localPath = args[1]
		}

		return downloadSharedFolder(ctx, cc, clients, item, localPath)
	}

	localPath := item.Name
	if len(args) > 1 {
		localPath = args[1]
	}

	tm := driveops.NewTransferManager(
		clients.Transfer,
		clients.Transfer,
		nil,
		cc.Logger,
		driveops.WithDiskCheck(sharedMinFreeSpace(cc), driveops.DiskAvailable),
	)

	result, err := tm.DownloadToFile(
		ctx,
		driveid.New(cc.SharedTarget.Ref.RemoteDriveID),
		cc.SharedTarget.Ref.RemoteItemID,
		localPath,
		driveops.DownloadOpts{
			RemoteHash: item.QuickXorHash,
			RemoteSize: item.Size,
		},
	)
	if err != nil {
		partialPath := localPath + ".partial"
		if _, statErr := localpath.Stat(partialPath); statErr == nil {
			cc.Statusf("Partial download saved: %s\n", partialPath)
			cc.Statusf("Re-run the same command to resume.\n")
		}

		return fmt.Errorf("download file: %w", err)
	}

	if cc.Flags.JSON {
		return printGetJSON(cc.Output(), getJSONOutput{
			Path:         localPath,
			Size:         result.Size,
			HashVerified: result.HashVerified,
		})
	}

	cc.Statusf("Downloaded %s (%s)\n", localPath, formatSize(result.Size))

	return nil
}

func downloadSharedFolder(
	ctx context.Context,
	cc *CLIContext,
	clients *driveops.AccountClients,
	root *graph.Item,
	localPath string,
) error {
	state := &downloadState{
		childCache: make(map[string][]graph.Item),
		sem:        make(chan struct{}, defaultDownloadConcurrency),
	}

	if err := countSharedFiles(
		ctx,
		clients.Meta,
		driveid.New(cc.SharedTarget.Ref.RemoteDriveID),
		cc.SharedTarget.Ref.RemoteItemID,
		state,
	); err != nil {
		return err
	}

	tm := driveops.NewTransferManager(
		clients.Transfer,
		clients.Transfer,
		driveops.NewSessionStore(config.DefaultDataDir(), cc.Logger),
		cc.Logger,
		driveops.WithDiskCheck(sharedMinFreeSpace(cc), driveops.DiskAvailable),
	)

	state.result.Errors = append(state.result.Errors, state.countErrors...)

	downloadSharedRecursive(ctx, cc, tm, state, driveid.New(cc.SharedTarget.Ref.RemoteDriveID), root, localPath)
	state.wg.Wait()

	if cc.Flags.JSON {
		return printGetFolderJSON(cc.Output(), state.result)
	}

	cc.Statusf("Downloaded %d files, %d folders (%s)\n",
		len(state.result.Files), state.result.FoldersCreated, formatSize(state.result.TotalSize))

	if len(state.result.Errors) > 0 {
		return fmt.Errorf("%d errors during download", len(state.result.Errors))
	}

	return nil
}

func countSharedFiles(ctx context.Context, client *graph.Client, driveID driveid.ID, folderID string, state *downloadState) error {
	children, err := client.ListChildren(ctx, driveID, folderID)
	if err != nil {
		return fmt.Errorf("listing shared folder contents: %w", err)
	}

	for i := range children {
		if children[i].IsFolder {
			if subErr := countSharedFiles(ctx, client, driveID, children[i].ID, state); subErr != nil {
				state.countErrors = append(state.countErrors, subErr.Error())
			}
		} else {
			state.total++
		}
	}

	state.childCache[folderID] = children
	return nil
}

func downloadSharedRecursive(
	ctx context.Context,
	cc *CLIContext,
	tm *driveops.TransferManager,
	state *downloadState,
	driveID driveid.ID,
	folder *graph.Item,
	localPath string,
) {
	if err := localpath.MkdirAll(localPath, defaultDirPerm); err != nil {
		state.mu.Lock()
		state.result.Errors = append(state.result.Errors, fmt.Sprintf("creating %q: %v", localPath, err))
		state.mu.Unlock()
		return
	}

	state.mu.Lock()
	state.result.FoldersCreated++
	children := state.childCache[folder.ID]
	state.mu.Unlock()

	for i := range children {
		child := children[i]
		childLocalPath := filepath.Join(localPath, child.Name)

		if child.IsFolder {
			state.wg.Add(1)
			go func(child graph.Item, childLocalPath string) {
				defer state.wg.Done()
				downloadSharedRecursive(ctx, cc, tm, state, driveID, &child, childLocalPath)
			}(child, childLocalPath)
			continue
		}

		state.wg.Add(1)
		go func(child graph.Item, childLocalPath string) {
			defer state.wg.Done()

			select {
			case state.sem <- struct{}{}:
				defer func() { <-state.sem }()
			case <-ctx.Done():
				return
			}

			result, err := tm.DownloadToFile(ctx, driveID, child.ID, childLocalPath, driveops.DownloadOpts{
				RemoteHash: child.QuickXorHash,
				RemoteSize: child.Size,
			})

			state.mu.Lock()
			defer state.mu.Unlock()

			if err != nil {
				state.result.Errors = append(state.result.Errors, fmt.Sprintf("downloading %q: %v", childLocalPath, err))
				return
			}

			state.result.Files = append(state.result.Files, getJSONOutput{
				Path:         childLocalPath,
				Size:         result.Size,
				HashVerified: result.HashVerified,
			})
			state.result.TotalSize += result.Size
			state.done++
			cc.Statusf("Downloaded %d/%d files\n", state.done, state.total)
		}(child, childLocalPath)
	}
}

func sharedMinFreeSpace(cc *CLIContext) int64 {
	if cc.Cfg == nil || cc.Cfg.MinFreeSpace == "" {
		return 0
	}

	parsed, err := config.ParseSize(cc.Cfg.MinFreeSpace)
	if err != nil {
		return 0
	}

	return parsed
}
