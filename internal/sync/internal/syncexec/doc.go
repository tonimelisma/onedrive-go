// Package syncexec is the executor: the worker pool, the dependency graph,
// the scope state and every action's effect on disk and on OneDrive. It
// reaches the store through its own ExecutorStore interface and imports only
// syncdomain.
package syncexec
