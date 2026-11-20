package syncshortcut

// The alias mutation vocabulary: what a parent may do to a shortcut
// placeholder. The *Engine methods that perform the Graph call live with the
// engine shell; only the description of the operation is shortcut-owned.

// shortcutAliasMutationKind identifies a parent-owned mutation of a OneDrive
// shortcut placeholder inside the parent engine's namespace.
type shortcutAliasMutationKind string

const (
	ShortcutAliasMutationRename shortcutAliasMutationKind = "rename"
	ShortcutAliasMutationDelete shortcutAliasMutationKind = "delete"
)

// ShortcutAliasMutation is intentionally scoped to one shortcut placeholder by
// binding item ID. It is not a discovery API and cannot address content inside
// the child target.
type ShortcutAliasMutation struct {
	Kind              shortcutAliasMutationKind
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
}
