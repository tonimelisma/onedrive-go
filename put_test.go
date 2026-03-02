package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitParentAndName(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantParent string
		wantName   string
	}{
		{"nested path", "foo/bar/baz", "foo/bar", "baz"},
		{"single segment", "baz", "", "baz"},
		{"empty string", "", "", ""},
		{"trailing slash top-level", "/top/", "", "top"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, name := splitParentAndName(tt.path)
			assert.Equal(t, tt.wantParent, parent)
			assert.Equal(t, tt.wantName, name)
		})
	}
}
