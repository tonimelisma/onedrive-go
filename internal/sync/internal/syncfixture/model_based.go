package syncfixture

import (
	"strings"
)

// ModelEdit is one operation a side applied, recorded for the invariants.
type ModelEdit struct {
	Side    string
	Kind    string
	Path    string
	To      string // destination of a move
	Content string // bytes written by create/edit
}

// Touches reports whether the edit affected p, its ancestors or its subtree.
func (e ModelEdit) Touches(p string) bool {
	for _, affected := range []string{e.Path, e.To} {
		if affected == "" {
			continue
		}
		if p == affected || strings.HasPrefix(p, affected+"/") || strings.HasPrefix(affected, p+"/") {
			return true
		}
	}

	return false
}

// ModelOverwrittenLater reports whether the same side later replaced or
// removed what edits[i] wrote, in which case losing it is correct.
func ModelOverwrittenLater(edits []ModelEdit, i int) bool {
	for _, later := range edits[i+1:] {
		if later.Side != edits[i].Side {
			continue
		}
		if later.Touches(edits[i].Path) && later.Kind != ModelOpMakeFolder {
			return true
		}
	}

	return false
}

const (
	ModelFolderSentinel = "/"
	ModelLocalSide      = "local"
	ModelRemoteSide     = "remote"
	ModelOpCreateFile   = "create-file"
	ModelOpEditFile     = "edit-file"
	ModelOpDelete       = "delete"
	ModelOpMove         = "move"
	ModelOpMakeFolder   = "mkdir"
	ModelOpReplace      = "replace"
)
