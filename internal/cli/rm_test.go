package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-1.4.3
func TestPrintRmJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printRmJSON(&buf, rmJSONOutput{Deleted: "/docs/old-report.pdf"})
	require.NoError(t, err)

	var decoded rmJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "/docs/old-report.pdf", decoded.Deleted)
}
