// Package syncdomain is the sync engine's vocabulary: row and action types,
// enums and their parsers, sentinel errors, scope keys, the content filter,
// the sync-root identity, the clock and IO interfaces and the truth-status
// index. Every other package of the sync tree reads it; it imports none of
// them.
package syncdomain
