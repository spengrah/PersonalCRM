'use client'

import { Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import {
  RefreshCw,
  Mail,
  Phone,
  Building2,
  Briefcase,
  UserPlus,
  Link2,
  X,
  CheckCircle,
  AlertCircle,
  CloudDownload,
  Calendar,
  Send,
  MessageCircle,
  Users,
} from 'lucide-react'
import { Navigation } from '@/components/layout/navigation'
import { Button } from '@/components/ui/button'
import { Pagination } from '@/components/ui/pagination'
import { SuggestionModal, type SuggestionModalItem } from '@/components/imports/suggestion-modal'
import { MethodSuggestionCard } from '@/components/imports/method-suggestion-card'
import { SubTabs, type ImportsTab } from '@/components/imports/interactions/SubTabs'
import { ConflictCard } from '@/components/imports/interactions/ConflictCard'
import { OrphanCard } from '@/components/imports/interactions/OrphanCard'
import { InteractionsEmptyState } from '@/components/imports/interactions/InteractionsEmptyState'
import { NameCandidateRow } from '@/components/imports/interactions/NameCandidateRow'
import { NameCandidateModal } from '@/components/imports/interactions/NameCandidateModal'
import { useIgnoreCandidate, useTriggerSync } from '@/hooks/use-imports'
import { useSuggestions, useDismissMethodSuggestions } from '@/hooks/use-suggestions'
import {
  useInteractionsQueue,
  useResolveLink,
  useAnarlogTitleCandidates,
  useResolveNameCandidate,
} from '@/hooks/use-interactions-queue'
import { useGoogleAccounts } from '@/hooks/use-google-accounts'
import { getCandidateDisplayName } from '@/lib/candidate-display'
import { sourceAllowsImport } from '@/lib/candidate-actions'
import type {
  ImportCandidate,
  SuggestionItem,
  SuggestionsListParams,
  MethodSuggestion,
  NeedsAttentionItem,
  NeedsAttentionCandidate,
  NameCandidateGroup,
} from '@/types/import'

// Constants
const DEFAULT_PAGE_SIZE = 20
const SOURCE_FILTERS = [
  { value: '', label: 'All Sources' },
  { value: 'gcontacts', label: 'Google Contacts' },
  { value: 'gcal_attendee', label: 'Calendar' },
  { value: 'icloud_contacts', label: 'iCloud Contacts' },
  { value: 'telegram', label: 'Telegram' },
  { value: 'anarlog_humans', label: 'Anarlog' },
] as const

/** Normalize the inbound `?tab` param to a canonical tab. `needs-attention`
 * is a transitional alias for `interactions`; everything else (including
 * `people` and unknown values) resolves to People. */
function normalizeTab(raw: string | null): ImportsTab {
  if (raw === 'interactions' || raw === 'needs-attention') return 'interactions'
  return 'people'
}

// Trusted domains for photo URLs (Google profile photos)
const TRUSTED_PHOTO_DOMAINS = ['googleusercontent.com', 'google.com', 'gstatic.com']

function isPhotoUrlTrusted(url: string): boolean {
  try {
    const hostname = new URL(url).hostname
    return TRUSTED_PHOTO_DOMAINS.some(domain => hostname.endsWith(domain))
  } catch {
    return false
  }
}

/** Header summary for the unified People tab: counts the method-suggestion
 * group and the candidate group, which are different kinds of work
 * (confirm methods vs import/link a contact). */
function headerSummary(methodCount: number, candidateTotal: number): string {
  const parts: string[] = []
  if (methodCount > 0) {
    parts.push(`${methodCount} method suggestion${methodCount > 1 ? 's' : ''}`)
  }
  if (candidateTotal > 0) {
    parts.push(`${candidateTotal} contact${candidateTotal > 1 ? 's' : ''} to review`)
  }
  return parts.join(' · ')
}

// Inline notification component
function Notification({
  type,
  message,
  onDismiss,
}: {
  type: 'success' | 'error'
  message: string
  onDismiss: () => void
}) {
  useEffect(() => {
    const timeout = setTimeout(onDismiss, 5000)
    return () => clearTimeout(timeout)
  }, [onDismiss])

  return (
    <div
      className={`mb-6 p-4 rounded-lg flex items-start space-x-3 ${
        type === 'success'
          ? 'bg-green-50 border border-green-200'
          : 'bg-red-50 border border-red-200'
      }`}
    >
      {type === 'success' ? (
        <CheckCircle className="w-5 h-5 text-green-600 flex-shrink-0 mt-0.5" />
      ) : (
        <AlertCircle className="w-5 h-5 text-red-600 flex-shrink-0 mt-0.5" />
      )}
      <p className={`flex-1 text-sm ${type === 'success' ? 'text-green-800' : 'text-red-800'}`}>
        {message}
      </p>
      <button onClick={onDismiss} className="text-gray-400 hover:text-gray-600">
        <X className="w-4 h-4" />
      </button>
    </div>
  )
}

// Candidate card component
function CandidateCard({
  candidate,
  onImport,
  onLink,
  onIgnore,
  importLoading,
  ignoreLoading,
}: {
  candidate: ImportCandidate
  onImport: () => void
  onLink: () => void
  onIgnore: () => void
  importLoading: boolean
  ignoreLoading: boolean
}) {
  const displayName = getCandidateDisplayName(candidate)

  // Get meeting context for calendar attendees
  const meetingContext =
    candidate.source === 'gcal_attendee' && candidate.metadata ? candidate.metadata : null

  // Validate meeting link is a safe HTTPS URL
  const safeMeetingLink = meetingContext?.meeting_link?.startsWith('https://')
    ? meetingContext.meeting_link
    : null

  // Gmail-correspondence evidence: the co-occurring contact and how many
  // messages this address was seen in.
  const correspondence =
    candidate.source === 'gmail_correspondence' && candidate.metadata ? candidate.metadata : null
  const correspondenceLabel = correspondence
    ? [
        correspondence.co_occurring_contact?.name
          ? `Seen with ${correspondence.co_occurring_contact.name}`
          : null,
        correspondence.message_count
          ? `${correspondence.message_count} ${correspondence.message_count === 1 ? 'message' : 'messages'}`
          : null,
      ]
        .filter(Boolean)
        .join(' · ')
    : null

  return (
    <div className="p-4 bg-white border border-gray-200 rounded-lg hover:shadow-sm transition-shadow">
      <div className="flex items-start justify-between">
        {/* Left side: Avatar and info */}
        <div className="flex items-start space-x-4">
          {/* Avatar */}
          {candidate.photo_url && isPhotoUrlTrusted(candidate.photo_url) ? (
            <img
              src={candidate.photo_url}
              alt={displayName}
              className="h-12 w-12 rounded-full object-cover"
            />
          ) : (
            <div className="h-12 w-12 rounded-full bg-gray-200 flex items-center justify-center">
              <span className="text-lg font-medium text-gray-600">
                {displayName.charAt(0).toUpperCase()}
              </span>
            </div>
          )}

          {/* Info */}
          <div className="flex-1 min-w-0">
            <div className="flex items-center flex-wrap gap-2">
              <h3 className="text-base font-medium text-gray-900">{displayName}</h3>
              {/* Inline meeting context badge for calendar attendees */}
              {meetingContext &&
                meetingContext.meeting_title &&
                (safeMeetingLink ? (
                  <a
                    href={safeMeetingLink}
                    target="_blank"
                    rel="noopener noreferrer"
                    aria-label={`View calendar event: ${meetingContext.meeting_title}`}
                    className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-blue-100 text-blue-800 hover:bg-blue-200 transition-colors"
                  >
                    <Calendar className="w-3 h-3 mr-1" />
                    From: {meetingContext.meeting_title}
                  </a>
                ) : (
                  <span className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-blue-100 text-blue-800">
                    <Calendar className="w-3 h-3 mr-1" />
                    From: {meetingContext.meeting_title}
                  </span>
                ))}
              {/* Inline correspondence-evidence badge for gmail_correspondence
                  candidates: who this address co-appeared with + message count. */}
              {correspondenceLabel && (
                <span className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-purple-100 text-purple-800">
                  <Users className="w-3 h-3 mr-1" />
                  {correspondenceLabel}
                </span>
              )}
            </div>

            {/* Organization and job title */}
            {(candidate.organization || candidate.job_title) && (
              <div className="mt-1 flex flex-wrap items-center gap-y-1 text-sm text-gray-600">
                {candidate.organization && (
                  <span className="flex items-center">
                    <Building2 className="w-3.5 h-3.5 mr-1 text-gray-400" />
                    {candidate.organization}
                  </span>
                )}
                {candidate.organization && candidate.job_title && (
                  <span className="mx-2 text-gray-300">·</span>
                )}
                {candidate.job_title && (
                  <span className="flex items-center">
                    <Briefcase className="w-3.5 h-3.5 mr-1 text-gray-400" />
                    {candidate.job_title}
                  </span>
                )}
              </div>
            )}

            {/* Contact info */}
            <div className="mt-2 flex flex-wrap gap-2">
              {candidate.emails.slice(0, 2).map((email, idx) => (
                <a
                  key={idx}
                  href={`mailto:${encodeURIComponent(email)}`}
                  title={email}
                  className="inline-flex items-center px-2 py-0.5 rounded bg-gray-100 text-sm text-gray-700 hover:bg-blue-50 hover:text-blue-600 transition-colors max-w-[300px]"
                >
                  <Mail className="w-3.5 h-3.5 mr-1.5 flex-shrink-0 text-gray-400" />
                  <span className="truncate">{email}</span>
                </a>
              ))}
              {candidate.phones.slice(0, 2).map((phone, idx) => (
                <a
                  key={idx}
                  href={`tel:${encodeURIComponent(phone)}`}
                  className="inline-flex items-center px-2 py-0.5 rounded bg-gray-100 text-sm text-gray-700 hover:bg-blue-50 hover:text-blue-600 transition-colors"
                >
                  <Phone className="w-3.5 h-3.5 mr-1.5 text-gray-400" />
                  {phone}
                </a>
              ))}
              {candidate.source === 'telegram' &&
                candidate.metadata?.username &&
                displayName !== candidate.metadata.username && (
                  <a
                    href={`https://t.me/${encodeURIComponent(candidate.metadata.username.replace(/^@/, ''))}`}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center px-2 py-0.5 rounded bg-gray-100 text-sm text-gray-700 hover:bg-blue-50 hover:text-blue-600 transition-colors"
                  >
                    <Send className="w-3.5 h-3.5 mr-1.5 text-gray-400" />
                    {candidate.metadata.username}
                  </a>
                )}
            </div>
          </div>
        </div>

        {/* Right side: Actions */}
        <div className="flex items-center space-x-2 ml-4">
          {/* Import is hidden for link-only sources (server policy mirror —
              gated on the source, not a response field). */}
          {sourceAllowsImport(candidate.source) && (
            <Button size="sm" onClick={onImport} loading={importLoading} disabled={ignoreLoading}>
              <UserPlus className="w-4 h-4 mr-1" />
              Import
            </Button>
          )}
          <Button
            size="sm"
            variant="outline"
            onClick={onLink}
            disabled={importLoading || ignoreLoading}
          >
            <Link2 className="w-4 h-4 mr-1" />
            {candidate.suggested_match
              ? `Link to ${candidate.suggested_match.contact_name} (${Math.round(candidate.suggested_match.confidence * 100)}%)`
              : 'Link (select)'}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={onIgnore}
            loading={ignoreLoading}
            disabled={importLoading}
            className="text-gray-500 hover:text-gray-700"
            aria-label="Ignore candidate"
          >
            <X className="w-4 h-4" />
          </Button>
        </div>
      </div>
    </div>
  )
}

export default function ImportsPage() {
  // useSearchParams requires a Suspense boundary in the App Router.
  return (
    <Suspense fallback={<ImportsPageFallback />}>
      <ImportsPageInner />
    </Suspense>
  )
}

function ImportsPageFallback() {
  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />
      <div className="max-w-5xl mx-auto py-6 sm:px-6 lg:px-8">
        <div className="space-y-4">
          {[...Array(5)].map((_, i) => (
            <div key={i} className="h-24 bg-gray-200 rounded-lg animate-pulse"></div>
          ))}
        </div>
      </div>
    </div>
  )
}

function ImportsPageInner() {
  const router = useRouter()
  const searchParams = useSearchParams()
  const tabParam = searchParams.get('tab')
  const sessionParam = searchParams.get('session')
  const activeTab = normalizeTab(tabParam)

  const [params, setParams] = useState<SuggestionsListParams>({
    page: 1,
    limit: DEFAULT_PAGE_SIZE,
  })
  const [notification, setNotification] = useState<{
    type: 'success' | 'error'
    message: string
  } | null>(null)
  // The unified modal opens on either a contact candidate (with array
  // navigation) or a method suggestion (enrich-locked single target).
  const [modalItem, setModalItem] = useState<SuggestionModalItem | null>(null)
  const [actionInProgress, setActionInProgress] = useState<string | null>(null)
  // Name-candidate modal: index into the name-candidate queue, or null when closed.
  const [nameCandidateModalIndex, setNameCandidateModalIndex] = useState<number | null>(null)
  // Token whose ignore/resolve mutation is in flight (disables its row).
  const [nameCandidateBusyToken, setNameCandidateBusyToken] = useState<string | null>(null)
  // Meeting-note id whose resolve mutation is in flight (disables its card).
  const [resolveBusyId, setResolveBusyId] = useState<string | null>(null)

  const { data, isLoading, error } = useSuggestions(params)
  const { data: googleAccounts } = useGoogleAccounts()
  const ignoreMutation = useIgnoreCandidate()
  const dismissMethodMutation = useDismissMethodSuggestions()
  const syncMutation = useTriggerSync()

  // Split the unified item list into the method group (rendered first) and
  // the contact candidates (the array the contact-modal navigates).
  const methodSuggestions = useMemo<MethodSuggestion[]>(
    () =>
      (data?.items || [])
        .filter((it): it is Extract<SuggestionItem, { kind: 'method' }> => it.kind === 'method')
        .map(it => it.suggestion),
    [data?.items]
  )
  const candidateItems = useMemo<ImportCandidate[]>(
    () =>
      (data?.items || [])
        .filter((it): it is Extract<SuggestionItem, { kind: 'contact' }> => it.kind === 'contact')
        .map(it => it.candidate),
    [data?.items]
  )

  const { data: attentionItems, isLoading: attentionLoading } = useInteractionsQueue()
  const { data: nameCandidateGroups, isLoading: nameCandidateLoading } = useAnarlogTitleCandidates()
  const resolveLinkMutation = useResolveLink()
  const resolveNameCandidateMutation = useResolveNameCandidate()

  const items = useMemo(() => attentionItems ?? [], [attentionItems])
  const groups = useMemo(() => nameCandidateGroups ?? [], [nameCandidateGroups])
  // Amber badge = conflicts + orphans (every needs-attention row). Name
  // candidates are deliberately NOT counted here.
  const attentionCount = items.length

  const setTab = useCallback(
    (tab: ImportsTab) => {
      const next = new URLSearchParams(searchParams.toString())
      next.set('tab', tab)
      // Switching tabs is a user action that should not re-trigger the
      // session deep-link, so drop any lingering session param.
      next.delete('session')
      router.replace(`/imports?${next.toString()}`)
    },
    [router, searchParams]
  )

  // Highlighted card (briefly emphasized after a `?session` deep-link).
  const [highlightedSession, setHighlightedSession] = useState<string | null>(null)
  const cardRefs = useRef<Record<string, HTMLDivElement | null>>({})

  // Normalize the transitional `needs-attention` alias to the canonical
  // `interactions` value in the URL on mount so a bookmark/share captures
  // the canonical param.
  useEffect(() => {
    if (tabParam === 'needs-attention') {
      const next = new URLSearchParams(searchParams.toString())
      next.set('tab', 'interactions')
      router.replace(`/imports?${next.toString()}`)
    }
    // Only re-run when the raw param changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tabParam])

  // Consume the `?session=<uuid>` deep-link: scroll to + highlight the
  // matching Interactions card, then strip the param so a refresh does not
  // re-trigger it (mirrors the one-time-action query-param pattern).
  useEffect(() => {
    if (!sessionParam || activeTab !== 'interactions') return
    if (attentionLoading) return
    const match = items.find(i => i.anarlog_session_id === sessionParam)
    if (match) {
      setHighlightedSession(match.id)
      cardRefs.current[match.id]?.scrollIntoView({ behavior: 'smooth', block: 'center' })
      const fade = setTimeout(() => setHighlightedSession(null), 2500)
      // Strip the consumed session param without touching the tab.
      const next = new URLSearchParams(searchParams.toString())
      next.delete('session')
      router.replace(`/imports?${next.toString()}`)
      return () => clearTimeout(fade)
    }
    // Re-run when the deep-link inputs or the loaded item set changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionParam, activeTab, attentionLoading, items])

  // Open the contact-candidate body at the given index (within the candidate
  // group, not the unified item list).
  const openCandidateModal = (index: number, mode: 'import' | 'link' = 'import') => {
    setModalItem({
      kind: 'contact',
      candidates: candidateItems,
      initialIndex: index,
      initialMode: mode,
    })
  }

  // Open the enrich-locked method-suggestion body for a single suggestion.
  const openMethodModal = (suggestion: MethodSuggestion) => {
    setModalItem({ kind: 'method', suggestion })
  }

  const closeModal = () => {
    setModalItem(null)
  }

  const handleDismissMethodSuggestion = async (suggestion: MethodSuggestion) => {
    setActionInProgress(suggestion.external_contact_id)
    try {
      await dismissMethodMutation.mutateAsync({
        id: suggestion.external_contact_id,
        request: {},
      })
      setNotification({
        type: 'success',
        message: `Dismissed suggestions for ${suggestion.contact_name}`,
      })
    } catch (error) {
      setNotification({
        type: 'error',
        message: error instanceof Error ? error.message : 'Failed to dismiss suggestions',
      })
    } finally {
      setActionInProgress(null)
    }
  }

  const handleIgnore = async (candidate: ImportCandidate) => {
    const displayName = getCandidateDisplayName(candidate)

    setActionInProgress(candidate.id)
    try {
      await ignoreMutation.mutateAsync(candidate.id)
      setNotification({
        type: 'success',
        message: `${displayName} ignored`,
      })
    } catch (error) {
      const errorMessage =
        error instanceof Error ? error.message : `Failed to ignore ${displayName}`
      setNotification({
        type: 'error',
        message: errorMessage,
      })
    } finally {
      setActionInProgress(null)
    }
  }

  const handleSyncContacts = async () => {
    // Check if there are any Google accounts connected
    if (!googleAccounts || googleAccounts.length === 0) {
      setNotification({
        type: 'error',
        message: 'No Google accounts connected. Please connect a Google account in Settings.',
      })
      return
    }

    try {
      // Sync all connected Google accounts
      for (const account of googleAccounts) {
        await syncMutation.mutateAsync({ source: 'gcontacts', accountId: account.account_id })
      }
      setNotification({
        type: 'success',
        message: `Contacts sync started for ${googleAccounts.length} account${googleAccounts.length > 1 ? 's' : ''}!`,
      })
    } catch (error) {
      // Extract error message from API response
      let errorMessage = 'Failed to start contacts sync. Please try again.'
      if (error instanceof Error) {
        errorMessage = error.message
      }

      // Provide more specific guidance for common errors
      if (errorMessage.includes('decrypt') || errorMessage.includes('authentication failed')) {
        errorMessage =
          'Your Google account connection has expired. Please reconnect your account in Settings.'
      } else if (errorMessage.includes('refresh token')) {
        errorMessage =
          'Unable to refresh your Google account. Please reconnect your account in Settings.'
      } else if (errorMessage.includes('OAuth')) {
        errorMessage = 'Authentication error. Please reconnect your Google account in Settings.'
      }

      setNotification({
        type: 'error',
        message: errorMessage,
      })
    }
  }

  const handleSyncCalendar = async () => {
    // Check if there are any Google accounts connected
    if (!googleAccounts || googleAccounts.length === 0) {
      setNotification({
        type: 'error',
        message: 'No Google accounts connected. Please connect a Google account in Settings.',
      })
      return
    }

    try {
      // Sync all connected Google accounts
      for (const account of googleAccounts) {
        await syncMutation.mutateAsync({ source: 'gcal', accountId: account.account_id })
      }
      setNotification({
        type: 'success',
        message: `Calendar sync started for ${googleAccounts.length} account${googleAccounts.length > 1 ? 's' : ''}!`,
      })
    } catch (error) {
      let errorMessage = 'Failed to start calendar sync. Please try again.'
      if (error instanceof Error) {
        errorMessage = error.message
      }

      if (errorMessage.includes('decrypt') || errorMessage.includes('authentication failed')) {
        errorMessage =
          'Your Google account connection has expired. Please reconnect your account in Settings.'
      } else if (errorMessage.includes('refresh token')) {
        errorMessage =
          'Unable to refresh your Google account. Please reconnect your account in Settings.'
      } else if (errorMessage.includes('OAuth')) {
        errorMessage = 'Authentication error. Please reconnect your Google account in Settings.'
      }

      setNotification({
        type: 'error',
        message: errorMessage,
      })
    }
  }

  const handleSourceFilter = (source: string) => {
    setParams(prev => ({
      ...prev,
      page: 1,
      source: source || undefined,
    }))
  }

  // --- Interactions tab: conflict / orphan resolution ---

  const handlePickCandidate = async (
    item: NeedsAttentionItem,
    candidate: NeedsAttentionCandidate
  ) => {
    setResolveBusyId(item.id)
    try {
      await resolveLinkMutation.mutateAsync({
        id: item.id,
        request: { action: 'link', kind: candidate.kind, id: candidate.id },
      })
      setNotification({ type: 'success', message: `${item.title || 'Session'} linked` })
    } catch (err) {
      setNotification({
        type: 'error',
        message: err instanceof Error ? err.message : 'Failed to resolve session',
      })
    } finally {
      setResolveBusyId(null)
    }
  }

  const handleLogImpromptu = async (item: NeedsAttentionItem) => {
    setResolveBusyId(item.id)
    try {
      await resolveLinkMutation.mutateAsync({
        id: item.id,
        request: { action: 'none_of_these' },
      })
      setNotification({
        type: 'success',
        message: `${item.title || 'Session'} logged as impromptu`,
      })
    } catch (err) {
      setNotification({
        type: 'error',
        message: err instanceof Error ? err.message : 'Failed to resolve session',
      })
    } finally {
      setResolveBusyId(null)
    }
  }

  // --- People tab: name-candidate token resolution ---

  const handleIgnoreToken = async (group: NameCandidateGroup) => {
    setNameCandidateBusyToken(group.normalized_token)
    try {
      await resolveNameCandidateMutation.mutateAsync({
        normalized_token: group.normalized_token,
        action: 'ignore',
      })
      setNotification({ type: 'success', message: `${group.token_display} ignored` })
    } catch (err) {
      setNotification({
        type: 'error',
        message: err instanceof Error ? err.message : 'Failed to ignore name',
      })
    } finally {
      setNameCandidateBusyToken(null)
    }
  }

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-5xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <div className="flex items-center space-x-3">
              <CloudDownload className="w-8 h-8 text-blue-600" />
              <h2 className="text-2xl font-bold leading-normal text-gray-900 sm:text-3xl sm:truncate">
                Import Contacts
              </h2>
            </div>
            <p className="mt-2 text-sm text-gray-500">
              {isLoading
                ? 'Loading...'
                : methodSuggestions.length > 0 || (data?.total ?? 0) > 0
                  ? headerSummary(methodSuggestions.length, data?.total ?? 0)
                  : 'Nothing to review'}
            </p>
          </div>
          <div className="mt-4 flex md:mt-0 md:ml-4 space-x-2">
            <Button variant="outline" onClick={handleSyncContacts} loading={syncMutation.isPending}>
              <RefreshCw className="w-4 h-4 mr-2" />
              Sync Contacts
            </Button>
            <Button variant="outline" onClick={handleSyncCalendar} loading={syncMutation.isPending}>
              <Calendar className="w-4 h-4 mr-2" />
              Sync Calendar
            </Button>
          </div>
        </div>

        {/* Sub-tabs (People / Interactions) — driven by the ?tab param */}
        <SubTabs active={activeTab} attentionCount={attentionCount} onChange={setTab} />

        {/* Notification (shared across tabs) */}
        {notification && (
          <Notification
            type={notification.type}
            message={notification.message}
            onDismiss={() => setNotification(null)}
          />
        )}

        {activeTab === 'interactions' ? (
          <InteractionsTab
            items={items}
            loading={attentionLoading}
            busyId={resolveBusyId}
            highlightedSession={highlightedSession}
            cardRefs={cardRefs}
            onPick={handlePickCandidate}
            onLogImpromptu={handleLogImpromptu}
          />
        ) : (
          <>
            {/* Source filter (People tab only) */}
            <div className="mb-6 flex items-center gap-2">
              <span className="text-sm text-gray-500">Filter:</span>
              {SOURCE_FILTERS.map(filter => (
                <button
                  key={filter.value}
                  onClick={() => handleSourceFilter(filter.value)}
                  className={`px-3 py-1.5 text-sm rounded-full transition-colors ${
                    (params.source || '') === filter.value
                      ? 'bg-blue-600 text-white'
                      : 'bg-gray-100 text-gray-700 hover:bg-gray-200'
                  }`}
                >
                  {filter.label}
                </button>
              ))}
            </div>

            {/* Error state */}
            {error && (
              <div className="bg-red-50 border border-red-200 rounded-md p-4 mb-6">
                <div className="flex">
                  <AlertCircle className="h-5 w-5 text-red-400" />
                  <div className="ml-3">
                    <h3 className="text-sm font-medium text-red-800">
                      Error loading import candidates
                    </h3>
                    <p className="mt-1 text-sm text-red-700">
                      {error instanceof Error ? error.message : 'An unexpected error occurred'}
                    </p>
                  </div>
                </div>
              </div>
            )}

            {/* Loading state */}
            {isLoading && (
              <div className="space-y-4">
                {[...Array(5)].map((_, i) => (
                  <div key={i} className="h-24 bg-gray-200 rounded-lg animate-pulse"></div>
                ))}
              </div>
            )}

            {/* Empty state */}
            {!isLoading &&
              !error &&
              methodSuggestions.length === 0 &&
              candidateItems.length === 0 && (
                <div className="text-center py-12 bg-white rounded-lg border border-gray-200">
                  <CloudDownload className="mx-auto h-12 w-12 text-gray-400" />
                  <h3 className="mt-2 text-sm font-medium text-gray-900">No import candidates</h3>
                  <p className="mt-1 text-sm text-gray-500">
                    All contacts from Google have been imported or are already linked.
                  </p>
                  <div className="mt-6 flex justify-center space-x-2">
                    <Button
                      variant="outline"
                      onClick={handleSyncContacts}
                      loading={syncMutation.isPending}
                    >
                      <RefreshCw className="w-4 h-4 mr-2" />
                      Sync Contacts
                    </Button>
                    <Button
                      variant="outline"
                      onClick={handleSyncCalendar}
                      loading={syncMutation.isPending}
                    >
                      <Calendar className="w-4 h-4 mr-2" />
                      Sync Calendar
                    </Button>
                  </div>
                </div>
              )}

            {/* Suggestions list: method group first (page 1 only, server
                orders it on top), then the confidence-ranked candidates. */}
            {!isLoading &&
              !error &&
              (methodSuggestions.length > 0 || candidateItems.length > 0) && (
                <div className="space-y-3">
                  {methodSuggestions.map(suggestion => (
                    <MethodSuggestionCard
                      key={suggestion.external_contact_id}
                      suggestion={suggestion}
                      onReview={() => openMethodModal(suggestion)}
                      onDismiss={() => handleDismissMethodSuggestion(suggestion)}
                      dismissLoading={
                        actionInProgress === suggestion.external_contact_id &&
                        dismissMethodMutation.isPending
                      }
                    />
                  ))}
                  {candidateItems.map((candidate, index) => (
                    <CandidateCard
                      key={candidate.id}
                      candidate={candidate}
                      onImport={() => openCandidateModal(index, 'import')}
                      onLink={() => openCandidateModal(index, 'link')}
                      onIgnore={() => handleIgnore(candidate)}
                      importLoading={false}
                      ignoreLoading={actionInProgress === candidate.id && ignoreMutation.isPending}
                    />
                  ))}
                </div>
              )}

            {/* Pagination */}
            {data && data.pages > 1 && (
              <div className="mt-6">
                <Pagination
                  page={data.page}
                  pages={data.pages}
                  total={data.total}
                  onPageChange={p => setParams(prev => ({ ...prev, page: p }))}
                />
              </div>
            )}

            {/* Name candidates: names found in session titles */}
            <NameCandidateSection
              groups={groups}
              loading={nameCandidateLoading}
              busyToken={nameCandidateBusyToken}
              onCreate={group =>
                setNameCandidateModalIndex(
                  groups.findIndex(g => g.normalized_token === group.normalized_token)
                )
              }
              onIgnore={handleIgnoreToken}
            />
          </>
        )}
      </div>

      {/* Unified suggestion modal (contact candidate OR method suggestion) */}
      {modalItem && (
        <SuggestionModal
          item={modalItem}
          onClose={closeModal}
          onSuccess={message => setNotification({ type: 'success', message })}
          onError={message => setNotification({ type: 'error', message })}
        />
      )}

      {/* Name-candidate modal (People-tab token groups) */}
      {nameCandidateModalIndex !== null && groups.length > 0 && (
        <NameCandidateModal
          groups={groups}
          initialIndex={Math.min(nameCandidateModalIndex, groups.length - 1)}
          onClose={() => setNameCandidateModalIndex(null)}
          onSuccess={message => setNotification({ type: 'success', message })}
          onError={message => setNotification({ type: 'error', message })}
        />
      )}
    </div>
  )
}

// --- Interactions tab (conflict + orphan queue) ---

function InteractionsTab({
  items,
  loading,
  busyId,
  highlightedSession,
  cardRefs,
  onPick,
  onLogImpromptu,
}: {
  items: NeedsAttentionItem[]
  loading: boolean
  busyId: string | null
  highlightedSession: string | null
  cardRefs: React.MutableRefObject<Record<string, HTMLDivElement | null>>
  onPick: (item: NeedsAttentionItem, candidate: NeedsAttentionCandidate) => void
  onLogImpromptu: (item: NeedsAttentionItem) => void
}) {
  if (loading) {
    return (
      <div className="space-y-4">
        {[...Array(3)].map((_, i) => (
          <div key={i} className="h-32 bg-gray-200 rounded-lg animate-pulse"></div>
        ))}
      </div>
    )
  }

  if (items.length === 0) {
    return <InteractionsEmptyState />
  }

  return (
    <div className="space-y-4">
      {items.map(item => {
        const isOrphan = (item.candidates ?? []).length === 0
        return (
          <div
            key={item.id}
            ref={el => {
              cardRefs.current[item.id] = el
            }}
            className={`rounded-lg transition-shadow ${
              highlightedSession === item.id ? 'ring-2 ring-amber-400 ring-offset-2' : ''
            }`}
          >
            {isOrphan ? (
              <OrphanCard item={item} busy={busyId === item.id} onLogImpromptu={onLogImpromptu} />
            ) : (
              <ConflictCard
                item={item}
                busy={busyId === item.id}
                onPick={onPick}
                onLogImpromptu={onLogImpromptu}
              />
            )}
          </div>
        )
      })}
    </div>
  )
}

// --- People tab: name-candidate section (names found in session titles) ---

function NameCandidateSection({
  groups,
  loading,
  busyToken,
  onCreate,
  onIgnore,
}: {
  groups: NameCandidateGroup[]
  loading: boolean
  busyToken: string | null
  onCreate: (group: NameCandidateGroup) => void
  onIgnore: (group: NameCandidateGroup) => void
}) {
  // Hide the section entirely when there are no name candidates (keeps the
  // People tab quiet for the common empty case).
  if (loading || groups.length === 0) return null

  return (
    <div className="mt-10">
      <div className="mb-3 flex items-center gap-2">
        <MessageCircle className="h-4 w-4 text-gray-400" />
        <h3 className="text-sm font-semibold text-gray-900">Names found in session titles</h3>
        <span className="text-xs text-gray-400">low confidence · from Anarlog</span>
      </div>
      <div className="space-y-2.5">
        {groups.map(group => (
          <NameCandidateRow
            key={group.normalized_token}
            group={group}
            busy={busyToken === group.normalized_token}
            onCreate={onCreate}
            onIgnore={onIgnore}
          />
        ))}
      </div>
    </div>
  )
}
