package sync

import (
	"strings"
)

// modelEdit is one operation a side applied, recorded for the invariants.
type modelEdit struct {
	side    string
	kind    string
	path    string
	to      string // destination of a move
	content string // bytes written by create/edit
}

// touches reports whether the edit affected p, its ancestors or its subtree.
func (e modelEdit) touches(p string) bool {
	for _, affected := range []string{e.path, e.to} {
		if affected == "" {
			continue
		}
		if p == affected || strings.HasPrefix(p, affected+"/") || strings.HasPrefix(affected, p+"/") {
			return true
		}
	}

	return false
}

// modelOverwrittenLater reports whether the same side later replaced or
// removed what edits[i] wrote, in which case losing it is correct.
func modelOverwrittenLater(edits []modelEdit, i int) bool {
	for _, later := range edits[i+1:] {
		if later.side != edits[i].side {
			continue
		}
		if later.touches(edits[i].path) && later.kind != modelOpMakeFolder {
			return true
		}
	}

	return false
}

const (
	modelFolderSentinel = "/"
	modelLocalSide      = "local"
	modelRemoteSide     = "remote"
	modelOpCreateFile   = "create-file"
	modelOpEditFile     = "edit-file"
	modelOpDelete       = "delete"
	modelOpMove         = "move"
	modelOpMakeFolder   = "mkdir"
	modelOpReplace      = "replace"
)
