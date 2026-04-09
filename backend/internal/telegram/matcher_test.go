package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildDisplayName(t *testing.T) {
	tests := []struct {
		name      string
		firstName *string
		lastName  *string
		want      *string
	}{
		{"both names", ptr("John"), ptr("Doe"), ptr("John Doe")},
		{"first only", ptr("John"), nil, ptr("John")},
		{"last only", nil, ptr("Doe"), ptr("Doe")},
		{"neither", nil, nil, nil},
		{"empty first", ptr(""), nil, nil},
		{"empty both", ptr(""), ptr(""), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDisplayName(tt.firstName, tt.lastName)
			if tt.want == nil {
				assert.Nil(t, got)
			} else {
				assert.Equal(t, *tt.want, *got)
			}
		})
	}
}

func ptr(s string) *string { return &s }
