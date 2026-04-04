package sharedref

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	ref, err := Parse("shared:alice@example.com:b!AbC123:01DEF456")
	require.NoError(t, err)

	assert.Equal(t, "alice@example.com", ref.AccountEmail)
	assert.Equal(t, "b!AbC123", ref.RemoteDriveID)
	assert.Equal(t, "01DEF456", ref.RemoteItemID)
	assert.Equal(t, "shared:alice@example.com:b!AbC123:01DEF456", ref.String())
}

func TestParse_Invalid(t *testing.T) {
	testCases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "missing prefix",
			input: "alice@example.com:b!abc123:01DEF456",
			want:  "must start with",
		},
		{
			name:  "too few parts",
			input: "shared:alice@example.com:b!abc123",
			want:  "must be",
		},
		{
			name:  "empty email",
			input: "shared::b!abc123:01DEF456",
			want:  "recipient email",
		},
		{
			name:  "empty drive id",
			input: "shared:alice@example.com::01DEF456",
			want:  "remote drive ID",
		},
		{
			name:  "empty item id",
			input: "shared:alice@example.com:b!abc123:",
			want:  "remote item ID",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := Parse(tc.input)
			require.Error(t, err)
			assert.True(t, ref.IsZero())
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestString_Zero(t *testing.T) {
	assert.Empty(t, Ref{}.String())
}
