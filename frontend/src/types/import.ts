/**
 * Types for import candidates from external sources (Google Contacts, iCloud, etc.)
 */

export interface SuggestedMatch {
  contact_id: string
  contact_name: string
  confidence: number
}

export interface ImportCandidateMetadata {
  meeting_title?: string
  meeting_date?: string
  meeting_link?: string
  discovered_at?: string
}

export interface ImportCandidate {
  id: string
  source: string
  account_id?: string
  display_name?: string
  first_name?: string
  last_name?: string
  organization?: string
  job_title?: string
  photo_url?: string
  emails: string[]
  phones: string[]
  suggested_match?: SuggestedMatch
  metadata?: ImportCandidateMetadata
}

export interface ImportCandidatesListParams {
  page?: number
  limit?: number
  source?: string
}

export interface ImportCandidatesListResponse {
  candidates: ImportCandidate[]
  total: number
  page: number
  limit: number
  pages: number
}

// Types for enhanced import/link with method selection

/** A contact method from an external source with original type info */
export interface ExternalContactMethod {
  value: string
  type: string // Original type from source (e.g., "work", "home", "other")
  primary?: boolean
}

/** User-selected method to import/link with assigned CRM type */
export interface SelectedMethod {
  original_value: string
  type: string // CRM type (email_personal, email_work, phone, etc.)
}

/** Request body for importing a contact with method selection */
export interface ImportContactRequest {
  selected_methods?: SelectedMethod[]
}

/** Request body for linking a contact with method selection and conflict resolution */
export interface LinkContactRequest {
  crm_contact_id: string
  selected_methods?: SelectedMethod[]
  conflict_resolutions?: Record<string, 'use_crm' | 'use_external'>
}

/** Type of conflict between external and CRM methods */
export type ConflictType = 'none' | 'identical' | 'type_conflict' | 'value_conflict'

/** Visual state for a method in the modal */
export type MethodState = 'unchanged' | 'adding' | 'conflict' | 'name_mismatch'

/** Comparison result between an external method and CRM methods */
export interface MethodComparison {
  external_value: string
  external_type: string
  suggested_crm_type: string
  crm_method?: {
    id: string
    type: string
    value: string
  }
  conflict_type: ConflictType
  state: MethodState
}
