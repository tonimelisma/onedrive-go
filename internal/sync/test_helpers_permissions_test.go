package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const (
	sharedTestFileItemID = "f1"
	sharedTestNFDResume  = "re\u0301sume\u0301.txt"
	sharedTestNFCResume  = "r\u00e9sum\u00e9.txt"
)

type mockPermChecker struct {
	perms map[string][]graph.Permission
	errs  map[string]error
}

func (m *mockPermChecker) ListItemPermissions(
	_ context.Context,
	driveID driveid.ID,
	itemID string,
) ([]graph.Permission, error) {
	key := driveID.String() + ":" + itemID
	if err := m.errs[key]; err != nil {
		return nil, err
	}

	return m.perms[key], nil
}
