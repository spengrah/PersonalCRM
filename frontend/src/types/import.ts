/**
 * Types for import candidates from external sources (Google Contacts, iCloud, etc.)
 */

export interface SuggestedMatch {
  contact_id: string
  contact_name: string
  confidence: number
}

export interface ImportCandidateMetadata {
  // Calendar attendee metadata (source: 'gcal_attendee')
  meeting_title?: string
  meeting_date?: string
  meeting_link?: string
  discovered_at?: string
  // Telegram peer metadata (source: 'telegram'). Stored with leading '@'.
  username?: string
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

// ---------------------------------------------------------------------------
// Unified suggestions surface (method group + confidence-ranked candidates)
// ---------------------------------------------------------------------------

export type SuggestionKind = 'contact' | 'method'
export type CandidateAction = 'import' | 'link' | 'ignore'

/** One pending (type, value) method on a method-suggestion. */
export interface MethodSuggestionMethod {
  type: string
  value: string
}

/** A method suggestion for an already-linked contact (confirm / dismiss). */
export interface MethodSuggestion {
  external_contact_id: string
  contact_id: string
  contact_name: string
  source: string
  methods: MethodSuggestionMethod[]
}

/** The contact variant wraps the existing ImportCandidate plus the
 * server-declared allowed actions (link-only policy). */
export interface CandidateSuggestion extends ImportCandidate {
  allowed_actions: CandidateAction[]
}

/** A queue entry in the unified suggestions list. */
export type SuggestionItem =
  | { kind: 'contact'; candidate: CandidateSuggestion }
  | { kind: 'method'; suggestion: MethodSuggestion }

export interface SuggestionsListParams {
  page?: number
  limit?: number
  source?: string
}

export interface SuggestionsListResponse {
  items: SuggestionItem[]
  // Candidate-group pagination meta (the method group rides above the fold
  // on page 1 and is excluded from the page math).
  total: number
  page: number
  limit: number
  pages: number
}

/** Request body for POST /imports/suggestions/:id/methods/resolve. */
export interface ResolveMethodSuggestionsRequest {
  methods?: MethodSuggestionMethod[]
}

/** Request body for POST /imports/suggestions/:id/methods/dismiss. */
export interface DismissMethodSuggestionsRequest {
  methods?: MethodSuggestionMethod[]
}

/** Response from POST /imports/suggestions/:id/methods/resolve. */
export interface ResolveMethodSuggestionsResponse {
  external_contact_id: string
  contact_id: string
  resolved_count: number
  rematch_job_id?: string | null
}

/** Response from POST /imports/suggestions/:id/methods/dismiss. */
export interface DismissMethodSuggestionsResponse {
  external_contact_id: string
  dismissed_count: number
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
  type: string // CRM type (email, phone, etc.)
  is_primary?: boolean // Whether this method should be the primary contact method
}

/** Request body for importing a contact with method selection */
export interface ImportContactRequest {
  selected_methods?: SelectedMethod[]
  cadence?: string
  name?: string // Optional custom name (overrides external source name)
}

/** Request body for linking a contact with method selection and conflict resolution */
export interface LinkContactRequest {
  crm_contact_id: string
  selected_methods?: SelectedMethod[]
  conflict_resolutions?: Record<string, 'use_crm' | 'use_external'>
  cadence?: string
  name?: string // Optional custom name (updates CRM contact name if provided)
  // True when the modal rendered the method-selection UI and the user made
  // a selection decision (even deselect-all). Closes the §4 residual where
  // a deselect-all link was classified `matched` instead of `imported`.
  methods_curated?: boolean
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

/** Response from POST /imports/:id/import — wraps the new contact and an optional rematch job ID. */
export interface ImportContactResponse {
  contact: import('./contact').Contact
  rematch_job_id?: string | null
}

/** Response from POST /imports/:id/link — wraps the linked external contact and an optional rematch job ID. */
export interface LinkContactResponse {
  external_contact: ImportCandidate
  rematch_job_id?: string | null
}

// ---------------------------------------------------------------------------
// Anarlog interactions-queue (Interactions tab) + name candidates (People tab) types
// ---------------------------------------------------------------------------

/** One attendee label on a conflict candidate. `matched` is a best-effort
 * flag for visual emphasis only; the authoritative shared count is
 * `overlap_count` on the candidate. */
export interface NeedsAttentionAttendee {
  name: string
  matched: boolean
}

/** Preview block for a single conflict candidate. */
export interface NeedsAttentionCandidatePreview {
  title?: string
  attendee_count?: number
  peer_handle?: string
  attendees?: NeedsAttentionAttendee[]
}

/** A single candidate within a conflict item's `candidates` array. */
export interface NeedsAttentionCandidate {
  kind: 'event' | 'phone_call'
  id: string
  occurred_at: string
  overlap_count: number
  target_missing: boolean
  preview: NeedsAttentionCandidatePreview | null
}

/** A row in the needs-attention response: a conflict (with candidates) or an
 * orphan (no candidates). */
export interface NeedsAttentionItem {
  id: string
  anarlog_session_id: string
  mac_host_id: string | null
  title: string | null
  summary_excerpt: string | null
  meeting_at: string
  linkage_state: string
  candidates: NeedsAttentionCandidate[] | null
}

/** Request body for POST /meeting-notes/:id/resolve-link. */
export interface ResolveLinkRequest {
  action: 'link' | 'none_of_these'
  kind?: 'event' | 'phone_call'
  id?: string
}

/** An interaction created by a resolve-link (carries the affected contact). */
export interface ResolveLinkInteraction {
  id: string
  contact_id: string
  source_ref: string
  occurred_at: string
  direction: string
}

/** Response from POST /meeting-notes/:id/resolve-link. */
export interface ResolveLinkResponse {
  meeting_note: {
    id: string
    anarlog_session_id: string
    title: string | null
    linkage_state: string
    linked_kind: string | null
    linked_id: string | null
    mac_host_id: string | null
    meeting_at: string
  }
  interactions_created: ResolveLinkInteraction[]
}

/** A grouped anarlog_title name candidate (People tab). */
export interface NameCandidateGroup {
  normalized_token: string
  token_display: string
  evidence_count: number
  session_titles: string[]
}

/** Request body for POST /imports/anarlog-title/resolve. */
export interface ResolveNameCandidateRequest {
  normalized_token: string
  action: 'import' | 'link' | 'ignore'
  name?: string
  cadence?: string
  crm_contact_id?: string
}

/** Response from POST /imports/anarlog-title/resolve. `contact_id` is present
 * for import (newly created) and link (the linked contact), omitted for ignore. */
export interface ResolveNameCandidateResponse {
  action: 'import' | 'link' | 'ignore'
  contact_id?: string
}
