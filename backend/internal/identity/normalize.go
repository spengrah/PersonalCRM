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
	// IdentifierTypeGChat is the Google Chat sender address. Chat sender
	// addresses ARE emails, so Normalize delegates to normalizeEmail
	// (lowercase + trim) — identical to the contact_method trigger's
	// handling of gchat values (migrations/021/022), so there is no
	// Go-vs-trigger drift.
	IdentifierTypeGChat IdentifierType = "gchat"
	// IdentifierTypeAnarlogHuman is the anarlog-internal human UUID
	// (UUID v4 string) emitted by the Mac daemon's anarlog_humans
	// plugin. Used as the lookup key for resolving
	// meeting_note.participant_ids into CRM contact_ids. Anarlog UUIDs
	// are not normalizable (already canonical) and do not search
	// against contact_method rows — the contact_id link comes from the
	// associated external_contact's email/phone match (or from a user
	// import via the Import handler).
	IdentifierTypeAnarlogHuman IdentifierType = "anarlog_human_id"
)

// ContactMethodType represents contact method types from the contact_method table
type ContactMethodType string

const (
	ContactMethodTypeEmail    ContactMethodType = "email"
	ContactMethodTypePhone    ContactMethodType = "phone"
	ContactMethodTypeTelegram ContactMethodType = "telegram"
	ContactMethodTypeWhatsApp ContactMethodType = "whatsapp"
	ContactMethodTypeGChat    ContactMethodType = "gchat"
)

// nonDigitRegex matches any non-digit character
var nonDigitRegex = regexp.MustCompile(`\D`)

// Normalize returns the normalized form of an identifier based on its type.
// Normalization rules:
//   - Email: lowercase, trim whitespace
//   - Phone: strip all non-digits, normalize to E.164 format
//   - Telegram: remove @ prefix, lowercase
//   - AnarlogHuman: trim whitespace, lowercase (UUIDs are case-insensitive;
//     lowercasing keeps lookups deterministic against the daemon's emit form)
func Normalize(raw string, idType IdentifierType) string {
	switch idType {
	case IdentifierTypeEmail, IdentifierTypeIMessageEmail, IdentifierTypeGChat:
		return normalizeEmail(raw)
	case IdentifierTypePhone, IdentifierTypeIMessagePhone, IdentifierTypeWhatsApp:
		return normalizePhone(raw)
	case IdentifierTypeTelegram:
		return normalizeTelegram(raw)
	case IdentifierTypeAnarlogHuman:
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return strings.TrimSpace(raw)
	}
}

// normalizeEmail normalizes an email address by lowercasing and trimming whitespace.
// Input is assumed to be validated upstream (handlers enforce email format).
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
// For email identifiers, we search email contact methods.
func MapIdentifierTypeToContactMethodTypes(idType IdentifierType) []ContactMethodType {
	switch idType {
	case IdentifierTypeEmail:
		return []ContactMethodType{ContactMethodTypeEmail}
	case IdentifierTypePhone:
		return []ContactMethodType{ContactMethodTypePhone}
	case IdentifierTypeTelegram:
		return []ContactMethodType{ContactMethodTypeTelegram}
	case IdentifierTypeIMessageEmail:
		return []ContactMethodType{ContactMethodTypeEmail}
	case IdentifierTypeIMessagePhone:
		return []ContactMethodType{ContactMethodTypePhone}
	case IdentifierTypeWhatsApp:
		return []ContactMethodType{ContactMethodTypeWhatsApp, ContactMethodTypePhone}
	// IdentifierTypeGChat is intentionally absent: no caller invokes
	// MatchOrCreate(IdentifierTypeGChat, ...) in the MVP, so a branch here
	// would be dead code and a second encoding of the (gchat, email)
	// dual-source set that could drift from the ListGChatIdentitiesForSync
	// sqlc query — that query is the single source of truth for the set.
	// If a future caller emerges, the WhatsApp branch above is the
	// precedent shape (gchat → [gchat, email]).
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
