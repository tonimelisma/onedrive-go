package syncverify

// Result describes the verification status of a single file.
type Result struct {
	Path     string `json:"path"`
	Status   string `json:"status"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// Report summarizes a full-tree hash verification of local files against the
// baseline database.
//
// Verified counts files whose content was actually compared. Files the
// baseline cannot describe well enough to check are reported separately
// rather than folded into that count: calling a file verified when nothing
// was compared answers "is my data intact" with evidence that does not exist.
type Report struct {
	Verified     int      `json:"verified"`
	Unverifiable []Result `json:"unverifiable,omitempty"`
	Mismatches   []Result `json:"mismatches"`
}
