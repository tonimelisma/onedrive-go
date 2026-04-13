// Package graphhttp owns Graph-facing HTTP client profile construction.
//
// It is intentionally narrower than internal/graph: graphhttp builds the
// concrete metadata and transfer *http.Client profiles, while graph.Client maps
// and normalizes Graph API behavior over those injected clients. Stateful
// runtime reuse and target-scoped throttle ownership live one layer up in
// driveops.SessionRuntime.
package graphhttp
