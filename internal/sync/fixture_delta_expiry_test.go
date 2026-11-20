package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// deltaGone is the 410 OneDrive answers for an expired delta token, with the
// resync flavor as its inner code ("" for a bare 410).
func deltaGone(innerCode string) error {
	graphErr := &graph.GraphError{StatusCode: 410, Code: "resyncRequired", Err: graph.ErrGone}
	if innerCode != "" {
		graphErr.InnerCodes = []string{innerCode}
	}

	return graphErr
}
