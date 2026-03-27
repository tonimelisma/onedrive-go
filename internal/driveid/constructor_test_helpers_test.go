package driveid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type constructorCase struct {
	name    string
	want    string
	wantErr bool
	call    func() (CanonicalID, error)
}

type parseCanonicalCase struct {
	name    string
	raw     string
	want    string
	wantErr bool
}

func assertConstructorCases(t *testing.T, tests []constructorCase) {
	t.Helper()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid, err := tt.call()
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cid.String())
		})
	}
}

func assertNewCanonicalIDCases(t *testing.T, tests []parseCanonicalCase) {
	t.Helper()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cid, err := NewCanonicalID(tt.raw)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cid.String())
		})
	}
}

func constructorSuccessCase(name, want string, call func() (CanonicalID, error)) constructorCase {
	return constructorCase{name: name, want: want, call: call}
}

func constructorErrorCase(name string, call func() (CanonicalID, error)) constructorCase {
	return constructorCase{name: name, wantErr: true, call: call}
}

func sharePointConstructorCases() []constructorCase {
	return []constructorCase{
		constructorSuccessCase("valid sharepoint", "sharepoint:alice@contoso.com:marketing:Documents",
			func() (CanonicalID, error) {
				return ConstructSharePoint("alice@contoso.com", "marketing", "Documents")
			}),
		constructorErrorCase("empty email", func() (CanonicalID, error) {
			return ConstructSharePoint("", "marketing", "Documents")
		}),
		constructorErrorCase("empty site", func() (CanonicalID, error) {
			return ConstructSharePoint("alice@contoso.com", "", "Documents")
		}),
		constructorErrorCase("empty library", func() (CanonicalID, error) {
			return ConstructSharePoint("alice@contoso.com", "marketing", "")
		}),
	}
}

func sharedConstructorCases() []constructorCase {
	return []constructorCase{
		constructorSuccessCase("valid shared", "shared:me@outlook.com:b!TG9yZW0:01ABCDEF",
			func() (CanonicalID, error) {
				return ConstructShared("me@outlook.com", "b!TG9yZW0", "01ABCDEF")
			}),
		constructorErrorCase("empty email", func() (CanonicalID, error) {
			return ConstructShared("", "b!TG9yZW0", "01ABCDEF")
		}),
		constructorErrorCase("empty source drive ID", func() (CanonicalID, error) {
			return ConstructShared("me@outlook.com", "", "01ABCDEF")
		}),
		constructorErrorCase("empty source item ID", func() (CanonicalID, error) {
			return ConstructShared("me@outlook.com", "b!TG9yZW0", "")
		}),
	}
}
