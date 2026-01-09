// Package identity provides identifier normalization and matching utilities
// for cross-platform contact identity matching.
package identity

import (
	"regexp"
	"strings"

	"personal-crm/backend/internal/matching"
)

// IdentifierType represents the type of external identifier
type IdentifierType string

const (
	IdentifierTypeEmail         IdentifierType = "email"
	IdentifierTypePhone         IdentifierType = "phone"
	IdentifierTypeTelegram      IdentifierType = "telegram"
	IdentifierTypeIMessageEmail IdentifierType = "imessage_email"
	IdentifierTypeIMessagePhone IdentifierType = "imessage_phone"
	IdentifierTypeWhatsApp      IdentifierType = "whatsapp"
)

// ContactMethodType represents contact method types from the contact_method table
type ContactMethodType string

const (
	ContactMethodTypeEmailPersonal ContactMethodType = "email_personal"
	ContactMethodTypeEmailWork     ContactMethodType = "email_work"
	ContactMethodTypePhone         ContactMethodType = "phone"
	ContactMethodTypeTelegram      ContactMethodType = "telegram"
	ContactMethodTypeWhatsApp      ContactMethodType = "whatsapp"
)

// nonDigitRegex matches any non-digit character
var nonDigitRegex = regexp.MustCompile(`\D`)

// Normalize returns the normalized form of an identifier based on its type.
// Normalization rules:
// - Email: lowercase, trim whitespace
// - Phone: strip all non-digits, normalize to E.164 format
// - Telegram: remove @ prefix, lowercase
func Normalize(raw string, idType IdentifierType) string {
	switch idType {
	case IdentifierTypeEmail, IdentifierTypeIMessageEmail:
		return normalizeEmail(raw)
	case IdentifierTypePhone, IdentifierTypeIMessagePhone, IdentifierTypeWhatsApp:
		return normalizePhone(raw)
	case IdentifierTypeTelegram:
		return normalizeTelegram(raw)
	default:
		return strings.TrimSpace(raw)
	}
}

// normalizeEmail normalizes an email address by lowercasing and trimming whitespace
func normalizeEmail(email string) string {
	return matching.NormalizeEmail(email)
}

// normalizePhone normalizes a phone number to E.164 format.
// It strips all non-digit characters and ensures proper country code handling.
func normalizePhone(phone string) string {
	return matching.NormalizePhoneE164(phone)
}

// normalizeTelegram normalizes a Telegram handle by removing @ prefix and lowercasing
func normalizeTelegram(handle string) string {
	handle = strings.TrimSpace(handle)
	handle = strings.TrimPrefix(handle, "@")
	return strings.ToLower(handle)
}

// MapIdentifierTypeToContactMethodTypes maps an external identifier type
// to the corresponding contact method types for matching.
// For email identifiers, we search both email_personal and email_work.
func MapIdentifierTypeToContactMethodTypes(idType IdentifierType) []ContactMethodType {
	switch idType {
	case IdentifierTypeEmail:
		return []ContactMethodType{ContactMethodTypeEmailPersonal, ContactMethodTypeEmailWork}
	case IdentifierTypePhone:
		return []ContactMethodType{ContactMethodTypePhone}
	case IdentifierTypeTelegram:
		return []ContactMethodType{ContactMethodTypeTelegram}
	case IdentifierTypeIMessageEmail:
		return []ContactMethodType{ContactMethodTypeEmailPersonal, ContactMethodTypeEmailWork}
	case IdentifierTypeIMessagePhone:
		return []ContactMethodType{ContactMethodTypePhone}
	case IdentifierTypeWhatsApp:
		return []ContactMethodType{ContactMethodTypeWhatsApp, ContactMethodTypePhone}
	default:
		return nil
	}
}

// DetectIdentifierType attempts to detect the type of an identifier based on its format.
// This is useful for iMessage which can use both email and phone.
func DetectIdentifierType(identifier string) IdentifierType {
	identifier = strings.TrimSpace(identifier)

	// Check for email format (contains @)
	if strings.Contains(identifier, "@") {
		return IdentifierTypeEmail
	}

	// Check for phone format (starts with + or is mostly digits)
	if strings.HasPrefix(identifier, "+") {
		return IdentifierTypePhone
	}

	// Count digits vs non-digits
	digits := nonDigitRegex.ReplaceAllString(identifier, "")
	if len(digits) >= 7 && float64(len(digits))/float64(len(identifier)) > 0.5 {
		return IdentifierTypePhone
	}

	// Default to email if we can't determine
	return IdentifierTypeEmail
}
