/**
 * Centralized query key definitions for React Query.
 *
 * All query keys should be defined here to:
 * 1. Avoid circular dependencies between hooks
 * 2. Provide a single source of truth for cache invalidation
 * 3. Enable type-safe query key references
 *
 * @see docs/FRONTEND_STATE.md for full documentation
 */

import type { ContactListParams } from '@/types/contact'
import type { ImportCandidatesListParams, SuggestionsListParams } from '@/types/import'

// Contact query keys
export const contactKeys = {
  all: ['contacts'] as const,
  lists: () => [...contactKeys.all, 'list'] as const,
  list: (params: ContactListParams) => [...contactKeys.lists(), params] as const,
  details: () => [...contactKeys.all, 'detail'] as const,
  detail: (id: string) => [...contactKeys.details(), id] as const,
  overdue: () => [...contactKeys.all, 'overdue'] as const,
}

// Import candidate query keys
export const importKeys = {
  all: ['imports'] as const,
  lists: () => [...importKeys.all, 'list'] as const,
  list: (params: ImportCandidatesListParams) => [...importKeys.lists(), params] as const,
  details: () => [...importKeys.all, 'detail'] as const,
  detail: (id: string) => [...importKeys.details(), id] as const,
  // Interactions tab: conflict + orphan queue (GET /meeting-notes/needs-attention).
  needsAttention: () => [...importKeys.all, 'needs-attention'] as const,
  // People tab: grouped anarlog_title name candidates (GET /imports/anarlog-title).
  anarlogTitle: () => [...importKeys.all, 'anarlog-title'] as const,
  // People tab: unified suggestions list (GET /imports/suggestions).
  // suggestionsLists() is a 2-element prefix that prefix-matches every
  // filtered variant — use it in invalidation rules (passing a params
  // object would become a literal mismatch against real filtered keys).
  suggestionsLists: () => [...importKeys.all, 'suggestions'] as const,
  suggestions: (params: SuggestionsListParams = {}) =>
    [...importKeys.suggestionsLists(), params] as const,
}

// System query keys
export const systemKeys = {
  all: ['system'] as const,
  time: () => [...systemKeys.all, 'time'] as const,
}

// Sync state query keys
export const syncKeys = {
  all: ['sync'] as const,
  states: () => [...syncKeys.all, 'states'] as const,
  state: (source: string, accountId?: string) =>
    [...syncKeys.all, 'state', source, accountId] as const,
  staleness: () => [...syncKeys.all, 'staleness'] as const,
}

// Contact task query keys
export const contactTaskKeys = {
  all: ['contact-tasks'] as const,
  lists: () => [...contactTaskKeys.all, 'list'] as const,
  // forContact returns a 3-element prefix that prefix-matches every
  // filtered variant for a given contact (state / kind / lifecycle
  // combinations all live under this prefix). Use this for invalidation
  // — passing `list(contactId)` with no params would push an explicit
  // `undefined` 4th slot, which React Query treats as a strict literal
  // and would NOT prefix-match real filtered keys.
  forContact: (contactId: string) => [...contactTaskKeys.lists(), contactId] as const,
  list: (contactId: string, params?: { state?: string; kind?: string; lifecycle?: string }) =>
    [...contactTaskKeys.lists(), contactId, params] as const,
}

export const telegramKeys = {
  all: ['telegram'] as const,
  status: () => [...telegramKeys.all, 'status'] as const,
  chats: () => [...telegramKeys.all, 'chats'] as const,
}

export const whatsappKeys = {
  all: ['whatsapp'] as const,
  status: () => [...whatsappKeys.all, 'status'] as const,
  chats: () => [...whatsappKeys.all, 'chats'] as const,
}

// Calendar event query keys
export const calendarKeys = {
  all: ['calendar-events'] as const,
  forContact: (contactId: string) => [...calendarKeys.all, 'contact', contactId] as const,
  upcomingForContact: (contactId: string) =>
    [...calendarKeys.all, 'upcoming', 'contact', contactId] as const,
  upcoming: () => [...calendarKeys.all, 'upcoming'] as const,
}

// Rematch job query keys
export const rematchKeys = {
  all: ['rematch'] as const,
  job: (jobId: string) => [...rematchKeys.all, 'job', jobId] as const,
}

// Mac host query keys (paired Mac daemon hosts).
export const macHostKeys = {
  all: ['mac-hosts'] as const,
  list: () => [...macHostKeys.all, 'list'] as const,
  detail: (id: string) => [...macHostKeys.all, 'detail', id] as const,
  sourceCounts: (id: string) => [...macHostKeys.all, 'source-counts', id] as const,
}
