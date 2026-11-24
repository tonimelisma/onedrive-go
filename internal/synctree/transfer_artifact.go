package synctree

import "strings"

// The names a download writes into a sync tree before it publishes the file:
// ".onedrive-go." + name (and a version tag) + ".partial". The transfer
// machinery (driveops) builds them; the sync content filter, the planner's
// delete safety and the CLI's error wording recognize them. The rule lives
// in this leaf so each reads the one definition.
const (
	TransferArtifactPrefix = ".onedrive-go."
	TransferArtifactSuffix = ".partial"
)

// IsOwnedTransferArtifactName reports whether a local basename belongs to
// onedrive-go's transfer machinery. It intentionally matches only the
// namespaced form used by this client, not arbitrary user-created *.partial
// files.
func IsOwnedTransferArtifactName(name string) bool {
	return strings.HasPrefix(name, TransferArtifactPrefix) && strings.HasSuffix(name, TransferArtifactSuffix)
}
