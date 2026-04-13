package service

import (
	"errors"
	"fmt"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestBuildMethodsFromExternal_Nil(t *testing.T) {
	assert.Nil(t, BuildMethodsFromExternal(nil))
}

func TestBuildMethodsFromExternal_Empty(t *testing.T) {
	got := BuildMethodsFromExternal(&repository.ExternalContact{Source: "telegram"})
	assert.Empty(t, got)
}

func TestBuildMethodsFromExternal_EmailsOnly(t *testing.T) {
	ext := &repository.ExternalContact{
		Source: "google",
		Emails: []repository.EmailEntry{
			{Value: "a@example.com"},
			{Value: "b@example.com"},
		},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Equal(t, []ContactMethodInput{
		{Type: "email", Value: "a@example.com"},
		{Type: "email", Value: "b@example.com"},
	}, got)
}

func TestBuildMethodsFromExternal_PhonesOnly(t *testing.T) {
	ext := &repository.ExternalContact{
		Source: "google",
		Phones: []repository.PhoneEntry{{Value: "+15551234"}},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Equal(t, []ContactMethodInput{{Type: "phone", Value: "+15551234"}}, got)
}

func TestBuildMethodsFromExternal_TelegramWithAt(t *testing.T) {
	ext := &repository.ExternalContact{
		Source:   "telegram",
		Metadata: map[string]any{"username": "@DaleDobeck"},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Equal(t, []ContactMethodInput{{Type: "telegram", Value: "DaleDobeck"}}, got)
}

func TestBuildMethodsFromExternal_TelegramWithoutAt(t *testing.T) {
	ext := &repository.ExternalContact{
		Source:   "telegram",
		Metadata: map[string]any{"username": "DaleDobeck"},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Equal(t, []ContactMethodInput{{Type: "telegram", Value: "DaleDobeck"}}, got)
}

func TestBuildMethodsFromExternal_TelegramEmptyUsername(t *testing.T) {
	ext := &repository.ExternalContact{
		Source:   "telegram",
		Metadata: map[string]any{"username": ""},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Empty(t, got)
}

func TestBuildMethodsFromExternal_TelegramWhitespaceOnly(t *testing.T) {
	ext := &repository.ExternalContact{
		Source:   "telegram",
		Metadata: map[string]any{"username": "@  "},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Empty(t, got)
}

func TestBuildMethodsFromExternal_TelegramMissingUsernameKey(t *testing.T) {
	ext := &repository.ExternalContact{
		Source:   "telegram",
		Metadata: map[string]any{"other_key": "foo"},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Empty(t, got)
}

func TestBuildMethodsFromExternal_TelegramNonStringUsername(t *testing.T) {
	ext := &repository.ExternalContact{
		Source:   "telegram",
		Metadata: map[string]any{"username": 42},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Empty(t, got)
}

func TestBuildMethodsFromExternal_NonTelegramSourceWithUsername(t *testing.T) {
	ext := &repository.ExternalContact{
		Source:   "google",
		Metadata: map[string]any{"username": "@shouldBeIgnored"},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Empty(t, got)
}

func TestBuildMethodsFromExternal_AllCombined_StableOrder(t *testing.T) {
	ext := &repository.ExternalContact{
		Source:   "telegram",
		Emails:   []repository.EmailEntry{{Value: "a@example.com"}},
		Phones:   []repository.PhoneEntry{{Value: "+15551234"}},
		Metadata: map[string]any{"username": "@handle"},
	}
	got := BuildMethodsFromExternal(ext)
	assert.Equal(t, []ContactMethodInput{
		{Type: "email", Value: "a@example.com"},
		{Type: "phone", Value: "+15551234"},
		{Type: "telegram", Value: "handle"},
	}, got)
}

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("some failure"), false},
		{"pg 23505 directly", &pgconn.PgError{Code: "23505"}, true},
		{"pg 23505 wrapped via fmt.Errorf %w", fmt.Errorf("create failed: %w", &pgconn.PgError{Code: "23505"}), true},
		{"pg 23503 (foreign key) is not 23505", &pgconn.PgError{Code: "23503"}, false},
		{"pg 23502 (not null) is not 23505", &pgconn.PgError{Code: "23502"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUniqueViolation(tt.err))
		})
	}
}

func TestCanonicalizeMethodValue(t *testing.T) {
	tests := []struct {
		name       string
		methodType string
		raw        string
		want       string
	}{
		{"telegram strips @", "telegram", "@handle", "handle"},
		{"telegram bare passthrough", "telegram", "handle", "handle"},
		{"telegram trims whitespace", "telegram", "  @handle  ", "handle"},
		{"telegram whitespace only becomes empty", "telegram", "@  ", ""},
		{"twitter strips @", "twitter", "@user", "user"},
		{"discord strips @", "discord", "@user", "user"},
		{"email passthrough preserves @", "email", "foo@example.com", "foo@example.com"},
		{"phone passthrough", "phone", "+15551234", "+15551234"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, canonicalizeMethodValue(tt.methodType, tt.raw))
		})
	}
}
