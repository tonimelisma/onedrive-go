package syncverify

// Result describes the verification status of a single file.
type Result struct {
	Path     string `json:"path"`
	Status   string `json:"status"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// Report summarizes a full-tree hash verification of local files against the baseline database.
type Report struct {
	Verified   int      `json:"verified"`
	Mismatches []Result `json:"mismatches"`
}
