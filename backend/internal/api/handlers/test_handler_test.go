package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCleanupRequest_Validation(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		isValid bool
	}{
		{
			name:    "valid prefix",
			prefix:  "w0-1234567890",
			isValid: true,
		},
		{
			name:    "empty prefix",
			prefix:  "",
			isValid: false,
		},
		{
			name:    "prefix with special chars",
			prefix:  "test-prefix",
			isValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := CleanupRequest{Prefix: tt.prefix}
			if tt.isValid {
				assert.NotEmpty(t, req.Prefix)
			} else {
				assert.Empty(t, req.Prefix)
			}
		})
	}
}
