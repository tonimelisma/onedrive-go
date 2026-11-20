package syncshortcut

// NewShortcutChildAckRefForTest is the acknowledgement token for the shortcut
// root bound to bindingItemID, which only sync issues outside tests. A
// ...ForTest function is used only by tests and the sync test-helper
// packages.
func NewShortcutChildAckRefForTest(bindingItemID string) ShortcutChildAckRef {
	return newShortcutChildAckRef(bindingItemID)
}
