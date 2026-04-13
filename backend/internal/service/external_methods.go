package service

import (
	"strings"

	"personal-crm/backend/internal/repository"
)

// BuildMethodsFromExternal converts an ExternalContact into the canonical
// list of ContactMethodInput entries. Single source of truth for translating
// source-specific fields (emails, phones, metadata.username) into the shape
// the contact / enrichment services consume.
//
// Callers are responsible for deduplicating against existing contact methods
// on the target contact — this function returns everything the external
// contact carries, in a stable order: emails, then phones, then telegram.
func BuildMethodsFromExternal(external *repository.ExternalContact) []ContactMethodInput {
	if external == nil {
		return nil
	}
	methods := make([]ContactMethodInput, 0, len(external.Emails)+len(external.Phones)+1)

	for _, email := range external.Emails {
		methods = append(methods, ContactMethodInput{Type: "email", Value: email.Value})
	}
	for _, phone := range external.Phones {
		methods = append(methods, ContactMethodInput{Type: "phone", Value: phone.Value})
	}

	if external.Source == "telegram" {
		if username, ok := external.Metadata["username"].(string); ok {
			canonical := canonicalizeMethodValue("telegram", username)
			if canonical != "" {
				methods = append(methods, ContactMethodInput{Type: "telegram", Value: canonical})
			}
		}
	}

	return methods
}

// canonicalizeMethodValue converts a user- or frontend-supplied raw method
// value into the canonical form stored in contact_method.value. For
// handle-based types (telegram/twitter/discord), strips leading '@' and
// trims whitespace. Does NOT lowercase — storage preserves user casing;
// value_normalized in the DB carries the lowercased form via the trigger.
func canonicalizeMethodValue(methodType, rawValue string) string {
	switch methodType {
	case "telegram", "twitter", "discord":
		return strings.TrimPrefix(strings.TrimSpace(rawValue), "@")
	default:
		return rawValue
	}
}
