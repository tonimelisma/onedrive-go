// Package syncplan is the planner: it turns the committed current state into
// the actions of one pass and their dependencies. It has no filesystem,
// network or database import, and imports only syncdomain.
package syncplan
