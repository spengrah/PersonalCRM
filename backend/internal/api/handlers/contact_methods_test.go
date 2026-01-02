package handlers

import (
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
)

func TestNormalizeContactMethodRequests(t *testing.T) {
	methods, err := normalizeContactMethodRequests([]ContactMethodRequest{
		{Type: " twitter ", Value: " @handle "},
		{Type: "telegram", Value: "   "},
		{Type: "email_personal", Value: "person@example.com"},
	})
	assert.NoError(t, err)
	assert.Len(t, methods, 2)
	assert.Equal(t, "twitter", methods[0].Type)
	assert.Equal(t, "handle", methods[0].Value)
	assert.Equal(t, "email_personal", methods[1].Type)
	assert.Equal(t, "person@example.com", methods[1].Value)
}

func TestValidateContactMethods_DuplicateTypes(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "email_personal", Value: "one@example.com"},
		{Type: "email_personal", Value: "two@example.com"},
	})
	assert.Error(t, err)
}

func TestValidateContactMethods_MultiplePrimary(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "email_personal", Value: "one@example.com", IsPrimary: true},
		{Type: "phone", Value: "+1-555-0100", IsPrimary: true},
	})
	assert.Error(t, err)
}

func TestValidateContactMethods_EmailValidation(t *testing.T) {
	validate := validator.New()
	err := validateContactMethods(validate, []ContactMethodRequest{
		{Type: "email_personal", Value: "not-an-email"},
	})
	assert.Error(t, err)
}

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
