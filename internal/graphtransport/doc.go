// Package graphtransport owns Graph-facing HTTP transport profile construction.
//
// It is intentionally narrower than internal/graph: graphtransport builds the
// concrete metadata and transfer *http.Client profiles, plus the sign-in
// service's auth client that the token sources and login flows use, while
// graph.Client maps and normalizes Graph API behavior over those injected
// clients. Stateful
// runtime reuse and target-scoped throttle ownership live one layer up in
// driveops.SessionRuntime.
package graphtransport
