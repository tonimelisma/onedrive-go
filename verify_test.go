package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync"
)

func TestPrintVerifyTable_NoMismatches(t *testing.T) {
	out := captureStdout(t, func() {
		printVerifyTable(&sync.VerifyReport{Verified: 10})
	})

	assert.Contains(t, out, "Verified: 10")
	assert.Contains(t, out, "All files verified successfully")
}

func TestPrintVerifyTable_WithMismatches(t *testing.T) {
	out := captureStdout(t, func() {
		printVerifyTable(&sync.VerifyReport{
			Verified: 8,
			Mismatches: []sync.VerifyResult{
				{Path: "/foo.txt", Status: "hash_mismatch", Expected: "aaa", Actual: "bbb"},
			},
		})
	})

	assert.Contains(t, out, "Mismatches: 1")
	assert.Contains(t, out, "/foo.txt")
}

func TestPrintVerifyJSON(t *testing.T) {
	out := captureStdout(t, func() {
		require.NoError(t, printVerifyJSON(&sync.VerifyReport{Verified: 5}))
	})

	assert.Contains(t, out, `"verified"`)

	var parsed sync.VerifyReport
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))
	assert.Equal(t, 5, parsed.Verified)
}

func TestNewVerifyCmd_Structure(t *testing.T) {
	t.Parallel()

	cmd := newVerifyCmd()
	assert.Equal(t, "verify", cmd.Use)
}
