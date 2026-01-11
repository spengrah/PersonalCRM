import freeEmailDomains from 'free-email-domains'
import type { ContactMethodType } from '@/types/contact'

// Create a Set for O(1) lookup
const FREE_EMAIL_DOMAINS = new Set(freeEmailDomains)

/**
 * Infers whether an email is personal or work based on domain.
 * Uses the free-email-domains package (4,779 domains from HubSpot) to detect
 * common personal email providers like Gmail, Yahoo, Outlook, etc.
 *
 * @param email - The email address to classify
 * @param originalType - Optional type hint from the source (e.g., "work", "home")
 * @returns 'email_personal' or 'email_work'
 */
export function inferEmailType(
  email: string,
  originalType?: string
): Extract<ContactMethodType, 'email_personal' | 'email_work'> {
  // First check original type hints from the source
  if (originalType) {
    const lowerType = originalType.toLowerCase()
    if (lowerType.includes('work') || lowerType === 'other') {
      return 'email_work'
    }
    if (lowerType.includes('personal') || lowerType.includes('home')) {
      return 'email_personal'
    }
  }

  // Extract domain from email
  const domain = email.split('@')[1]?.toLowerCase()
  if (!domain) {
    return 'email_personal'
  }

  // Check if domain is a known free email provider
  if (FREE_EMAIL_DOMAINS.has(domain)) {
    return 'email_personal'
  }

  // Assume corporate/custom domains are work emails
  return 'email_work'
}

/**
 * Checks if a domain is a known free email provider.
 */
export function isFreeEmailDomain(domain: string): boolean {
  return FREE_EMAIL_DOMAINS.has(domain.toLowerCase())
}
