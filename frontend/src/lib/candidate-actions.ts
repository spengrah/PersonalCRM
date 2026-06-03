import type { CandidateAction } from '@/types/import'

/**
 * Link-only sources: sources that may never create a NEW CRM contact.
 *
 * This is a PRESENTATION MIRROR of the server-side policy
 * (service.AllowedActionsForSource / IsLinkOnlySource in the Go backend,
 * which is the AUTHORITY and the enforcement). The modal and card gate the
 * Import affordance on this helper rather than on a response field, because
 * the modal's candidate array comes from a 1000-candidate refetch of
 * /imports/candidates that does NOT carry `allowed_actions`. Keep this set
 * in lock-step with the backend's linkOnlySources.
 */
const LINK_ONLY_SOURCES = new Set<string>(['gmail_correspondence'])

/**
 * Returns the actions a candidate of the given source permits. A link-only
 * source omits 'import' so it cannot seed a new contact.
 */
export function allowedActionsForSource(source: string): CandidateAction[] {
  if (LINK_ONLY_SOURCES.has(source)) {
    return ['link', 'ignore']
  }
  return ['import', 'link', 'ignore']
}

/** Convenience: whether the source allows importing as a new contact. */
export function sourceAllowsImport(source: string): boolean {
  return allowedActionsForSource(source).includes('import')
}
