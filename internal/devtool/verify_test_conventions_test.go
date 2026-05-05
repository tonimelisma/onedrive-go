package devtool

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.17
func TestRunTestConventionsRejectsStdlibTestingFailures(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "bad_test.go"),
		[]byte(`package sample

import "testing"

func TestBad(t *testing.T) {
	t.Fatal("stop")
	t.Errorf("bad")
}
`),
	))

	stdout := &bytes.Buffer{}
	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, stdout, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "test convention check failed")
	assert.Contains(t, err.Error(), "bad_test.go:6: direct t.Fatal")
	assert.Contains(t, err.Error(), "bad_test.go:7: direct t.Errorf")
	assert.Contains(t, stdout.String(), "==> test conventions")
}

// Validates: R-6.10.17
func TestRunTestConventionsRejectsDirectSleepOutsideLiveE2EHelper(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, "sleepy_test.go"),
		[]byte(`package sample

import (
	"testing"
	"time"
)

func TestSleepy(t *testing.T) {
	time.Sleep(time.Second)
}
`),
	))

	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sleepy_test.go:9: direct time.Sleep")
}

// Validates: R-6.10.17
func TestRunTestConventionsAllowsCentralizedLiveE2ESleepHelper(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	require.NoError(t, writeFile(
		filepath.Join(repoRoot, allowedDirectSleepTestFile),
		[]byte(`package e2e

import (
	"testing"
	"time"
)

func TestHelper(t *testing.T) {
	time.Sleep(time.Millisecond)
}
`),
	))

	err := runTestConventions(context.Background(), &fakeRunner{}, repoRoot, nil, &bytes.Buffer{}, &bytes.Buffer{})
	require.NoError(t, err)
}
