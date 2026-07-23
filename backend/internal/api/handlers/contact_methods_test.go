package handlers

import (
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
)

// spec: CON-015[0]
func TestNormalizeContactMethodRequests(t *testing.T) {
	methods, err := normalizeContactMethodRequests([]ContactMethodRequest{
		{Type: " twitter ", Value: " @handle "},
		{Type: "telegram", Value: "   "},
		{Type: "email", Value: "person@example.com"},
	})
	assert.NoError(t, err)
	assert.Len(t, methods, 2)
	assert.Equal(t, "twitter", methods[0].Type)
	assert.Equal(t, "handle", methods[0].Value)
	assert.Equal(t, "email", methods[1].Type)
	assert.Equal(t, "person@example.com", methods[1].Value)
}

func TestValidateContactMethods_DuplicateTypesAllowed(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "email", Value: "one@example.com"},
		{Type: "email", Value: "two@example.com"},
	})
	assert.NoError(t, err)
}

// spec: CON-015[5]
func TestValidateContactMethods_DuplicateNormalizedValuePerType(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "email", Value: "Person@Example.com"},
		{Type: "email", Value: " person@example.com "},
	})
	assert.Error(t, err)
}

// spec: CON-015[6]
func TestValidateContactMethods_MultiplePrimary(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "email", Value: "one@example.com", IsPrimary: true},
		{Type: "phone", Value: "+1-555-0100", IsPrimary: true},
	})
	assert.Error(t, err)
}

// spec: CON-015[3]
func TestValidateContactMethods_EmailValidation(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "email", Value: "not-an-email"},
	})
	assert.Error(t, err)
}

// spec: CON-015[4]
func TestValidateContactMethods_PhoneLength(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "phone", Value: strings.Repeat("1", 51)},
	})
	assert.Error(t, err)
}

func TestValidateContactMethods_WhatsAppValid(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "whatsapp", Value: "+1-555-123-4567"},
	})
	assert.NoError(t, err)
}

func TestValidateContactMethods_WhatsAppLength(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "whatsapp", Value: strings.Repeat("1", 51)},
	})
	assert.Error(t, err)
}
