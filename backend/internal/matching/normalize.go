// Package matching centralizes fuzzy matching normalization utilities.
package matching

import (
	"regexp"
	"strings"
)

var nonDigitRegex = regexp.MustCompile(`\D`)

// NormalizeEmail normalizes an email address by lowercasing and trimming whitespace.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// NormalizePhoneLoose strips formatting but preserves a leading + when present.
// This matches import matching behavior to avoid changing existing results.
func NormalizePhoneLoose(phone string) string {
	if phone == "" {
		return ""
	}

	var normalized strings.Builder
	hasLeadingPlus := strings.HasPrefix(phone, "+")

	for i, r := range phone {
		if r == '+' && i == 0 && hasLeadingPlus {
			normalized.WriteRune(r)
		} else if r >= '0' && r <= '9' {
			normalized.WriteRune(r)
		}
	}

	return normalized.String()
}

// NormalizePhoneE164 normalizes a phone number to E.164 format.
// It strips all non-digit characters and ensures proper country code handling.
func NormalizePhoneE164(phone string) string {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return ""
	}

	hasPlus := strings.HasPrefix(phone, "+")
	digits := nonDigitRegex.ReplaceAllString(phone, "")
	if digits == "" {
		return ""
	}

	if len(digits) == 10 && !hasPlus {
		return "+1" + digits
	}

	if len(digits) == 11 && digits[0] == '1' {
		return "+" + digits
	}

	if hasPlus {
		return "+" + digits
	}

	return "+" + digits
}
