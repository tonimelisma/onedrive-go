package devtool

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCoverageRequirement is the percentage the requirement fixture of the
// full-tier profile tests states. Their fake coverage report sits above it.
const testCoverageRequirement = "70.0"

// writeCoverageRequirement writes a requirements file whose coverage line
// states percent, carrying no status so the traceability rule asks no
// evidence of the temporary tree.
func writeCoverageRequirement(t *testing.T, repoRoot string, percent string) {
	t.Helper()

	require.NoError(t, writeFile(
		filepath.Join(repoRoot, specRequirementsDir, coverageRequirementFile),
		[]byte("- "+coverageRequirementID+": Total statement coverage, measured over all packages, "+
			"shall not drop below "+percent+"% in the verification gate.\n"),
	))
}

// Validates: R-6.10.4
//
// The gate's number is written once, in the requirement, and devtool reads it
// from there. Reading the repository's own requirements file proves the line
// devtool parses is the line the spec states, with exactly one percentage.
func TestRequiredCoverageReadsRequirement(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootForTest(t)

	required, err := requiredCoverage(repoRoot)
	require.NoError(t, err)
	assert.Positive(t, required)
	assert.LessOrEqual(t, required, maxCoveragePercent)

	data, err := readFile(filepath.Join(repoRoot, specRequirementsDir, coverageRequirementFile))
	require.NoError(t, err)

	var requirementLine string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "- "+coverageRequirementID+":") {
			requirementLine = line
		}
	}
	require.NotEmpty(t, requirementLine, "the requirements file states %s", coverageRequirementID)

	// The line may write a whole number as 84% or as 84.0%.
	shortest := strconv.FormatFloat(required, 'f', -1, 64) + "%"
	oneDecimal := strconv.FormatFloat(required, 'f', 1, 64) + "%"
	assert.True(t,
		strings.Contains(requirementLine, shortest) || strings.Contains(requirementLine, oneDecimal),
		"%s states %s, the number devtool read", coverageRequirementID, shortest)
}

// Validates: R-6.10.4
//
// A requirement the gate cannot read is a failure, never a silent fallback to
// some other number: that fallback is how devtool and CI came to gate on two
// different values.
func TestRequiredCoverageRejectsUnusableRequirement(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		contents string
		want     string
	}{
		{
			name:     "no requirement line",
			contents: "- R-6.10.3: Something else. [verified]\n",
			want:     "does not state " + coverageRequirementID,
		},
		{
			name:     "no percentage",
			contents: "- " + coverageRequirementID + ": Coverage shall not drop. [verified]\n",
			want:     "exactly one percentage, found 0",
		},
		{
			name:     "two percentages",
			contents: "- " + coverageRequirementID + ": Coverage shall not drop below 80.0% or 81.0%. [verified]\n",
			want:     "exactly one percentage, found 2",
		},
		{
			name:     "above one hundred",
			contents: "- " + coverageRequirementID + ": Coverage shall not drop below 180.0%. [verified]\n",
			want:     "out of range",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoRoot := t.TempDir()
			require.NoError(t, writeFile(
				filepath.Join(repoRoot, specRequirementsDir, coverageRequirementFile),
				[]byte(tc.contents),
			))

			_, err := requiredCoverage(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	_, err := requiredCoverage(t.TempDir())
	require.Error(t, err, "a tree without the requirements file has no number to gate on")
}

// Validates: R-6.10.4
func TestParseCoverageTotal(t *testing.T) {
	t.Parallel()

	total, err := parseCoverageTotal("pkg/foo\t80.0%\ntotal:\t(statements)\t76.2%\n")
	require.NoError(t, err)
	assert.InDelta(t, 76.2, total, 0.001)
}

// Validates: R-6.10.4
//
// The flag's zero value, which is also its default, means the number the
// requirement states: a report one tenth below it fails the gate.
func TestRunVerifyZeroCoverageThresholdUsesRequirement(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)
	writeCoverageRequirement(t, repoRoot, "80.0")

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t79.9%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:     repoRoot,
		Profile:      VerifyPublic,
		CoverageFile: filepath.Join(repoRoot, "cover.out"),
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coverage gate failed: 79.9% < 80.0%")
}

// Validates: R-6.10.4
//
// A positive threshold overrides the requirement and does not read it: the
// tree carries no requirements file.
func TestRunVerifyCoverageThresholdFailure(t *testing.T) {
	t.Parallel()

	const explicitThreshold = 90.0

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t89.9%\n"),
		},
	}

	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyPublic,
		CoverageThreshold: explicitThreshold,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            &bytes.Buffer{},
		Stderr:            &bytes.Buffer{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "coverage gate failed: 89.9% < 90.0%")
}

// Validates: R-6.10.4
//
// A negative threshold reports coverage without gating, so a secondary
// platform leg can run the same suite without owning the coverage number. It
// does not read the requirement either.
func TestRunVerifyNegativeCoverageThresholdReportsWithoutGating(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	writeRequirementsBackedReadme(t, repoRoot)

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"go tool cover -func=" + filepath.Join(repoRoot, "cover.out"): []byte("total:\t(statements)\t12.3%\n"),
		},
	}

	stdout := &bytes.Buffer{}
	err := RunVerify(context.Background(), runner, &VerifyOptions{
		RepoRoot:          repoRoot,
		Profile:           VerifyPublic,
		CoverageThreshold: -1,
		CoverageFile:      filepath.Join(repoRoot, "cover.out"),
		Stdout:            stdout,
		Stderr:            &bytes.Buffer{},
	})
	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "coverage gate disabled (total 12.3%)")
}
