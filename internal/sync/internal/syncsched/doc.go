// Package syncsched is the scheduler of one sync run: the dependency graph,
// which actions are queued and running, the slow start of a fresh plan, the
// work held for a retry or a block scope, the active block scopes and the
// sliding windows that detect them, and the admission decision that joins
// them. It is pure decisions and bookkeeping: no store, no timers, no
// channels, no goroutines and no locks. The engine's loop goroutine is its
// one caller and applies the effects (store writes, timers, dispatch).
// It imports only syncdomain.
package syncsched
