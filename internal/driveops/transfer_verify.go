package driveops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ErrUploadVerificationFailed reports an upload whose stored item does not
// match the bytes that were sent, outside the known server-enrichment class.
// It used to be logged at Info and the upload reported as a success, so the
// baseline recorded "local = what we sent, remote = what OneDrive stored" and
// neither side ever differed from its half again: a silent, permanent
// divergence (D-035). Interactive put/cp now fails, and sync keeps the action
// as retry work without committing a converged baseline.
var ErrUploadVerificationFailed = errors.New("OneDrive stored different content than was uploaded")

// ItemReader reads remote items back after a transfer. Satisfied by
// *graph.Client and type-asserted from the Uploader, like the other optional
// transfer capabilities.
type ItemReader interface {
	GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
}

// uploadTarget names where an upload's item lives: by item ID for an
// overwrite, by parent ID plus name for a create.
type uploadTarget struct {
	driveID  driveid.ID
	itemID   string
	parentID string
	name     string
}

// uploadedContent is what the upload sent.
type uploadedContent struct {
	size         int64
	quickXorHash string
	name         string
}

type transferVerdict int

const (
	transferVerified transferVerdict = iota
	transferEnrichedKnown
	transferMismatch
)

// verifyTransfer is the one decision for "did OneDrive store what we sent".
// Only QuickXorHash is comparable: it is the only digest computed locally,
// and it covers the length, so a matching hash with stale size metadata is
// still verified. Without one, the size is the only evidence. A difference
// is accepted only for the documented SharePoint/Business enrichment class
// (Office documents and media get server-side metadata); anything else is a
// mismatch.
func verifyTransfer(sent uploadedContent, item *graph.Item) transferVerdict {
	switch {
	case item.QuickXorHash != "" && item.QuickXorHash == sent.quickXorHash:
		return transferVerified
	case item.QuickXorHash == "" && item.Size == sent.size:
		return transferVerified
	case serverEnrichesContent(item, sent.name):
		return transferEnrichedKnown
	default:
		return transferMismatch
	}
}

// Business and SharePoint parentReference.driveType values: drives whose
// server rewrites uploaded Office and media files.
const (
	driveTypeBusiness        = "business"
	driveTypeDocumentLibrary = "documentLibrary"
)

// businessDriveIDPrefix starts every Business and SharePoint drive ID; a
// Personal drive ID is hex. It identifies the drive when Graph omits
// parentReference.driveType.
const businessDriveIDPrefix = "b!"

// isEnrichedExtension reports the file types SharePoint/Business is known to
// rewrite after upload (property promotion into Office documents, metadata
// into media), changing their hash and size.
func isEnrichedExtension(ext string) bool {
	switch ext {
	case
		".doc", ".docx", ".docm", ".dot", ".dotx", ".dotm",
		".xls", ".xlsx", ".xlsm", ".xlsb", ".xlt", ".xltx", ".xltm",
		".ppt", ".pptx", ".pptm", ".pot", ".potx", ".potm", ".pps", ".ppsx", ".ppsm",
		".vsd", ".vsdx", ".vsdm", ".one", ".pdf",
		".jpg", ".jpeg", ".png", ".gif", ".tif", ".tiff", ".heic", ".heif",
		".mp3", ".m4a", ".mp4", ".mov", ".m4v", ".wmv", ".avi":
		return true
	default:
		return false
	}
}

func serverEnrichesContent(item *graph.Item, name string) bool {
	if !isBusinessLikeDrive(item) {
		return false
	}

	return isEnrichedExtension(strings.ToLower(filepath.Ext(name)))
}

func isBusinessLikeDrive(item *graph.Item) bool {
	if item.ParentDriveType != "" {
		return strings.EqualFold(item.ParentDriveType, driveTypeBusiness) ||
			strings.EqualFold(item.ParentDriveType, driveTypeDocumentLibrary)
	}

	return strings.HasPrefix(item.DriveID.String(), businessDriveIDPrefix)
}

// settleUploadedItem turns an upload call's answer into a verified item.
// A completion that carried no item is looked up at the target, because the
// content has landed; an item that does not match what was sent is read back
// once (server processing can briefly show pre-commit facts) and the upload
// fails if it still does not match.
func (tm *TransferManager) settleUploadedItem(
	ctx context.Context, target uploadTarget, sent uploadedContent, localPath string, item *graph.Item, uploadErr error,
) (*graph.Item, error) {
	if uploadErr != nil {
		if graph.IsFinalFragmentUncertain(uploadErr) {
			// The encapsulated small-file session canceled itself, but its
			// last fragment may have committed: adopt the item if it holds
			// exactly these bytes (D-036).
			if landed, ok := tm.adoptLandedUpload(ctx, target, sent); ok {
				return landed, nil
			}

			return nil, uploadErr
		}

		if !errors.Is(uploadErr, graph.ErrUploadCompletedWithoutItem) {
			return nil, uploadErr
		}

		item = nil
	}

	if item == nil || item.ID == "" {
		looked, err := tm.lookupUploadTarget(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("uploading %s: the upload completed without the item, and looking it up failed: %w",
				localPath, err)
		}

		item = looked
	}

	switch verifyTransfer(sent, item) {
	case transferVerified:
		return item, nil
	case transferEnrichedKnown:
		// Debug only: SharePoint rewrites these on every upload, with
		// nothing for the user to do. The baseline records the stored facts
		// per side, so the next pass is quiet.
		tm.logger.Debug("OneDrive enriched the uploaded file",
			slog.String("path", localPath),
			slog.String("local_hash", sent.quickXorHash),
			slog.String("remote_hash", item.QuickXorHash),
		)

		return item, nil
	case transferMismatch:
	}

	reread, err := tm.lookupUploadTarget(ctx, uploadTarget{driveID: target.driveID, itemID: item.ID})
	if err == nil && verifyTransfer(sent, reread) != transferMismatch {
		return reread, nil
	}

	tm.logger.Debug("upload verification failed",
		slog.String("path", localPath),
		slog.Int64("local_size", sent.size),
		slog.Int64("remote_size", item.Size),
		slog.String("local_hash", sent.quickXorHash),
		slog.String("remote_hash", item.QuickXorHash),
	)

	return nil, fmt.Errorf("uploading %s: %w (sent %s, OneDrive has %s)",
		localPath, ErrUploadVerificationFailed, formatSize(sent.size), formatSize(item.Size))
}

// lookupUploadTarget reads the item an upload wrote: by ID, or by an exact
// name match in the parent's listing for a create.
func (tm *TransferManager) lookupUploadTarget(ctx context.Context, target uploadTarget) (*graph.Item, error) {
	reader, ok := tm.uploads.(ItemReader)
	if !ok {
		return nil, errors.New("uploader cannot read items back")
	}

	if target.itemID != "" {
		item, err := reader.GetItem(ctx, target.driveID, target.itemID)
		if err != nil {
			return nil, fmt.Errorf("reading item %s: %w", target.itemID, err)
		}

		return item, nil
	}

	children, err := reader.ListChildren(ctx, target.driveID, target.parentID)
	if err != nil {
		return nil, fmt.Errorf("listing parent %s: %w", target.parentID, err)
	}

	for i := range children {
		if !children[i].IsFolder && children[i].Name == target.name && children[i].ID != "" {
			return &children[i], nil
		}
	}

	return nil, fmt.Errorf("%q is not in its parent folder: %w", target.name, graph.ErrNotFound)
}
