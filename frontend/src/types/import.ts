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
