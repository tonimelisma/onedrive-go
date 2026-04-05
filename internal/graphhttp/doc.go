// Package graphhttp owns Graph-facing HTTP client construction and shared HTTP
// runtime state such as target-scoped retry throttle coordination.
//
// It is intentionally narrower than internal/graph: graphhttp builds client
// profiles, while graph.Client maps and normalizes Graph API behavior over the
// injected *http.Client values.
package graphhttp
