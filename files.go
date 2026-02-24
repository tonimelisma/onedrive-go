package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls [path]",
		Short: "List files and folders",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runLs,
	}
}

func newGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <remote-path> [local-path]",
		Short: "Download a file",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runGet,
	}
}

func newPutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "put <local-path> [remote-path]",
		Short: "Upload a file",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runPut,
	}
}

func newRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <path>",
		Short: "Delete a file or folder",
		Args:  cobra.ExactArgs(1),
		RunE:  runRm,
	}
}

func newMkdirCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mkdir <path>",
		Short: "Create a folder (recursive)",
		Args:  cobra.ExactArgs(1),
		RunE:  runMkdir,
	}
}

func newStatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stat <path>",
		Short: "Display file or folder metadata",
		Args:  cobra.ExactArgs(1),
		RunE:  runStat,
	}
}

// cleanRemotePath strips leading/trailing slashes, returns "" for root.
func cleanRemotePath(path string) string {
	return strings.Trim(path, "/")
}

// splitParentAndName splits a remote path into parent path and name.
// For "foo/bar/baz" returns ("foo/bar", "baz").
// For "baz" returns ("", "baz").
func splitParentAndName(path string) (string, string) {
	clean := cleanRemotePath(path)
	idx := strings.LastIndex(clean, "/")

	if idx < 0 {
		return "", clean
	}

	return clean[:idx], clean[idx+1:]
}

// clientAndDrive loads a saved token using the resolved config's canonical ID,
// creates a Graph client, and discovers the user's primary drive ID.
// Returns the client, drive ID, and logger for callers that need to log.
func clientAndDrive(ctx context.Context) (*graph.Client, driveid.ID, *slog.Logger, error) {
	logger := buildLogger()

	tokenPath := config.DriveTokenPath(resolvedCfg.CanonicalID)
	if tokenPath == "" {
		return nil, driveid.ID{}, nil, fmt.Errorf("cannot determine token path for drive %q", resolvedCfg.CanonicalID)
	}

	ts, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return nil, driveid.ID{}, nil, fmt.Errorf("not logged in — run 'onedrive-go login' first")
		}

		return nil, driveid.ID{}, nil, err
	}

	client := graph.NewClient(graph.DefaultBaseURL, defaultHTTPClient(), ts, logger)

	// Skip the Drives() API call when the drive ID is already known from config.
	if !resolvedCfg.DriveID.IsZero() {
		logger.Debug("using configured drive ID", "drive_id", resolvedCfg.DriveID.String())
		return client, resolvedCfg.DriveID, logger, nil
	}

	drives, err := client.Drives(ctx)
	if err != nil {
		return nil, driveid.ID{}, nil, fmt.Errorf("discovering drive: %w", err)
	}

	if len(drives) == 0 {
		return nil, driveid.ID{}, nil, fmt.Errorf("no drives found for this account")
	}

	logger.Debug("discovered primary drive", "drive_id", drives[0].ID.String())

	return client, drives[0].ID, logger, nil
}

// resolveItem resolves a remote path to an Item.
// For root (""), uses GetItem with "root". Otherwise uses GetItemByPath.
// Note: "/" normalizes to "" via cleanRemotePath, so callers can pass either
// "/" or "" to mean root. This is intentional — the CLI defaults to "/".
func resolveItem(ctx context.Context, client *graph.Client, driveID driveid.ID, remotePath string) (*graph.Item, error) {
	clean := cleanRemotePath(remotePath)
	if clean == "" {
		return client.GetItem(ctx, driveID, "root")
	}

	return client.GetItemByPath(ctx, driveID, clean)
}

// listItems lists children of a remote path.
// For root (""), uses ListChildren with "root". Otherwise uses ListChildrenByPath.
func listItems(ctx context.Context, client *graph.Client, driveID driveid.ID, remotePath string) ([]graph.Item, error) {
	clean := cleanRemotePath(remotePath)
	if clean == "" {
		return client.ListChildren(ctx, driveID, "root")
	}

	return client.ListChildrenByPath(ctx, driveID, clean)
}

func runLs(cmd *cobra.Command, args []string) error {
	remotePath := "/"
	if len(args) > 0 {
		remotePath = args[0]
	}

	ctx := cmd.Context()

	client, driveID, logger, err := clientAndDrive(ctx)
	if err != nil {
		return err
	}

	logger.Debug("ls", "path", remotePath)

	items, err := listItems(ctx, client, driveID, remotePath)
	if err != nil {
		return fmt.Errorf("listing %q: %w", remotePath, err)
	}

	if flagJSON {
		return printItemsJSON(items)
	}

	printItemsTable(items)

	return nil
}

// lsJSONItem is the JSON output schema for a single item in ls output.
type lsJSONItem struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsFolder   bool   `json:"is_folder"`
	ModifiedAt string `json:"modified_at"`
	ID         string `json:"id"`
}

func printItemsJSON(items []graph.Item) error {
	out := make([]lsJSONItem, 0, len(items))
	for i := range items {
		out = append(out, lsJSONItem{
			Name:       items[i].Name,
			Size:       items[i].Size,
			IsFolder:   items[i].IsFolder,
			ModifiedAt: items[i].ModifiedAt.Format("2006-01-02T15:04:05Z"),
			ID:         items[i].ID,
		})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

func printItemsTable(items []graph.Item) {
	// Sort: folders first, then alphabetical.
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsFolder != items[j].IsFolder {
			return items[i].IsFolder
		}

		return items[i].Name < items[j].Name
	})

	headers := []string{"NAME", "SIZE", "MODIFIED"}
	rows := make([][]string, 0, len(items))

	for i := range items {
		name := items[i].Name
		if items[i].IsFolder {
			name += "/"
		}

		rows = append(rows, []string{name, formatSize(items[i].Size), formatTime(items[i].ModifiedAt)})
	}

	printTable(os.Stdout, headers, rows)
}

func runGet(cmd *cobra.Command, args []string) error {
	remotePath := args[0]
	ctx := cmd.Context()

	client, driveID, logger, err := clientAndDrive(ctx)
	if err != nil {
		return err
	}

	logger.Debug("get", "remote_path", remotePath)

	item, err := resolveItem(ctx, client, driveID, remotePath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", remotePath, err)
	}

	if item.IsFolder {
		return fmt.Errorf("%q is a folder, not a file", remotePath)
	}

	localPath := item.Name
	if len(args) > 1 {
		localPath = args[1]
	}

	// Atomic download: write to a temp file in the same directory, then rename.
	// This prevents partial downloads from leaving corrupt files on disk.
	dir := filepath.Dir(localPath)

	tmp, err := os.CreateTemp(dir, ".download-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for download: %w", err)
	}

	tmpPath := tmp.Name()

	// Clean up temp file on any error path.
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	n, err := client.Download(ctx, driveID, item.ID, tmp)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("downloading %q: %w", remotePath, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpPath, localPath); err != nil {
		return fmt.Errorf("renaming download to %q: %w", localPath, err)
	}

	success = true

	logger.Debug("download complete", "local_path", localPath, "bytes", n)
	statusf("Downloaded %s (%s)\n", localPath, formatSize(n))

	return nil
}

func runPut(cmd *cobra.Command, args []string) error {
	localPath := args[0]
	ctx := cmd.Context()

	fi, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("stating local file: %w", err)
	}

	if fi.IsDir() {
		return fmt.Errorf("%q is a directory, not a file", localPath)
	}

	// Default remote path is root + local filename.
	remotePath := "/" + filepath.Base(localPath)
	if len(args) > 1 {
		remotePath = args[1]
	}

	client, driveID, logger, err := clientAndDrive(ctx)
	if err != nil {
		return err
	}

	logger.Debug("put", "local_path", localPath, "remote_path", remotePath, "size", fi.Size())

	parentPath, name := splitParentAndName(remotePath)

	// Resolve parent folder ID.
	parentItem, err := resolveItem(ctx, client, driveID, parentPath)
	if err != nil {
		return fmt.Errorf("resolving parent %q: %w", parentPath, err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("opening local file: %w", err)
	}
	defer f.Close()

	progress := func(uploaded, total int64) {
		statusf("Uploading: %s / %s\n", formatSize(uploaded), formatSize(total))
	}

	_, err = client.Upload(ctx, driveID, parentItem.ID, name, f, fi.Size(), fi.ModTime(), progress)
	if err != nil {
		return fmt.Errorf("uploading %q: %w", remotePath, err)
	}

	logger.Debug("upload complete", "remote_path", remotePath, "size", fi.Size())
	statusf("Uploaded %s (%s)\n", remotePath, formatSize(fi.Size()))

	return nil
}

// rmJSONOutput is the JSON output schema for the rm command.
type rmJSONOutput struct {
	Deleted string `json:"deleted"`
}

func runRm(cmd *cobra.Command, args []string) error {
	remotePath := args[0]
	ctx := cmd.Context()

	client, driveID, logger, err := clientAndDrive(ctx)
	if err != nil {
		return err
	}

	logger.Debug("rm", "path", remotePath)

	item, err := resolveItem(ctx, client, driveID, remotePath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", remotePath, err)
	}

	if err := client.DeleteItem(ctx, driveID, item.ID); err != nil {
		return fmt.Errorf("deleting %q: %w", remotePath, err)
	}

	logger.Debug("delete complete", "path", remotePath, "item_id", item.ID)

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(rmJSONOutput{Deleted: remotePath})
	}

	statusf("Deleted %s\n", remotePath)

	return nil
}

// mkdirJSONOutput is the JSON output schema for the mkdir command.
type mkdirJSONOutput struct {
	Created string `json:"created"`
	ID      string `json:"id"`
}

func runMkdir(cmd *cobra.Command, args []string) error {
	remotePath := cleanRemotePath(args[0])
	if remotePath == "" {
		return fmt.Errorf("cannot create root folder")
	}

	ctx := cmd.Context()

	client, driveID, logger, err := clientAndDrive(ctx)
	if err != nil {
		return err
	}

	logger.Debug("mkdir", "path", remotePath)

	// Walk path segments, creating each missing folder.
	segments := strings.Split(remotePath, "/")
	parentID := "root"
	builtPath := ""

	for _, seg := range segments {
		if builtPath == "" {
			builtPath = seg
		} else {
			builtPath = builtPath + "/" + seg
		}

		item, createErr := client.CreateFolder(ctx, driveID, parentID, seg)
		if createErr != nil {
			// If folder already exists (409 Conflict), resolve it and continue.
			if errors.Is(createErr, graph.ErrConflict) {
				existing, resolveErr := resolveItem(ctx, client, driveID, builtPath)
				if resolveErr != nil {
					return fmt.Errorf("resolving existing folder %q: %w", seg, resolveErr)
				}

				parentID = existing.ID

				continue
			}

			return fmt.Errorf("creating folder %q: %w", seg, createErr)
		}

		parentID = item.ID
	}

	logger.Debug("mkdir complete", "path", remotePath, "folder_id", parentID)

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(mkdirJSONOutput{Created: remotePath, ID: parentID})
	}

	statusf("Created %s\n", remotePath)

	return nil
}

func runStat(cmd *cobra.Command, args []string) error {
	remotePath := args[0]
	ctx := cmd.Context()

	client, driveID, logger, err := clientAndDrive(ctx)
	if err != nil {
		return err
	}

	logger.Debug("stat", "path", remotePath)

	item, err := resolveItem(ctx, client, driveID, remotePath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", remotePath, err)
	}

	if flagJSON {
		return printStatJSON(item)
	}

	printStatText(item)

	return nil
}

// statJSONOutput is the JSON output schema for the stat command.
type statJSONOutput struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsFolder   bool   `json:"is_folder"`
	ModifiedAt string `json:"modified_at"`
	CreatedAt  string `json:"created_at"`
	MimeType   string `json:"mime_type,omitempty"`
	ETag       string `json:"etag"`
}

func printStatJSON(item *graph.Item) error {
	out := statJSONOutput{
		ID:         item.ID,
		Name:       item.Name,
		Size:       item.Size,
		IsFolder:   item.IsFolder,
		ModifiedAt: item.ModifiedAt.Format("2006-01-02T15:04:05Z"),
		CreatedAt:  item.CreatedAt.Format("2006-01-02T15:04:05Z"),
		MimeType:   item.MimeType,
		ETag:       item.ETag,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

func printStatText(item *graph.Item) {
	itemType := "file"
	if item.IsFolder {
		itemType = "folder"
	}

	fmt.Printf("Name:     %s\n", item.Name)
	fmt.Printf("Type:     %s\n", itemType)
	fmt.Printf("Size:     %s (%d bytes)\n", formatSize(item.Size), item.Size)
	fmt.Printf("Modified: %s\n", item.ModifiedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Printf("Created:  %s\n", item.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Printf("ID:       %s\n", item.ID)

	if item.MimeType != "" {
		fmt.Printf("MIME:     %s\n", item.MimeType)
	}
}
