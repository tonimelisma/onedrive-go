package config

import "github.com/tonimelisma/onedrive-go/internal/driveid"

// accountCIDForDrive returns the account canonical ID that owns this drive.
// Personal and business drives own themselves. SharePoint drives belong to the
// business account with the same email. Shared drives are resolved separately
// from the catalog drive record.
func accountCIDForDrive(cid driveid.CanonicalID) driveid.CanonicalID {
	switch {
	case cid.IsPersonal(), cid.IsBusiness():
		return cid
	case cid.IsSharePoint():
		biz, err := driveid.Construct(driveid.DriveTypeBusiness, cid.Email())
		if err != nil {
			return driveid.CanonicalID{}
		}

		return biz
	default:
		return driveid.CanonicalID{}
	}
}
