package multisync

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func testCanonicalID(t *testing.T, s string) driveid.CanonicalID {
	t.Helper()

	cid, err := driveid.NewCanonicalID(s)
	require.NoError(t, err)

	return cid
}

func testStandaloneMountIdentity(cid driveid.CanonicalID) MountIdentity {
	return MountIdentity{
		MountID:        cid.String(),
		ProjectionKind: MountProjectionStandalone,
		CanonicalID:    cid,
	}
}
