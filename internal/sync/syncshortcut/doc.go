// Package syncshortcut holds the shortcut-root state machine and its
// deterministic planners, and the protected-root types. The store runs the
// planners inside its transactions and the engine carries their results out
// on disk; it imports only syncdomain.
package syncshortcut
