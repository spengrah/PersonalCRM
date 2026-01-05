'use client'

import { useState, useEffect } from 'react'
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
} from 'lucide-react'
import { Navigation } from '@/components/layout/navigation'
import { Button } from '@/components/ui/button'
import { ContactSelector } from '@/components/ui/contact-selector'
import { useContacts } from '@/hooks/use-contacts'
import {
  useImportCandidates,
  useImportAsContact,
  useLinkCandidate,
  useIgnoreCandidate,
  useTriggerSync,
} from '@/hooks/use-imports'
import { useGoogleAccounts } from '@/hooks/use-google-accounts'
import type { ImportCandidate, ImportCandidatesListParams } from '@/types/import'

// Constants
const DEFAULT_PAGE_SIZE = 20
const CONTACT_SELECTOR_LIMIT = 500

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

// Link to existing contact modal
function LinkContactModal({
  candidate,
  onLink,
  onCancel,
  loading,
}: {
  candidate: ImportCandidate
  onLink: (contactId: string) => void
  onCancel: () => void
  loading: boolean
}) {
  const [selectedContactId, setSelectedContactId] = useState<string | undefined>()
  const { data: contactsData } = useContacts({ limit: CONTACT_SELECTOR_LIMIT })

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (selectedContactId) {
      onLink(selectedContactId)
    }
  }

  const displayName =
    candidate.display_name ||
    [candidate.first_name, candidate.last_name].filter(Boolean).join(' ') ||
    'Unknown'

  return (
    <div className="fixed inset-0 bg-gray-600 bg-opacity-50 overflow-y-auto h-full w-full z-50">
      <div className="relative top-20 mx-auto p-6 border w-full max-w-lg shadow-lg rounded-lg bg-white">
        <div className="flex items-start justify-between mb-6">
          <div>
            <h3 className="text-lg font-medium text-gray-900">Link to Existing Contact</h3>
            <p className="mt-1 text-sm text-gray-500">
              Link <span className="font-medium">{displayName}</span> to an existing contact in your
              CRM. Their data will be used to enrich the existing contact.
            </p>
          </div>
          <button
            type="button"
            className="text-gray-400 hover:text-gray-600"
            onClick={onCancel}
            disabled={loading}
          >
            <X className="w-6 h-6" />
          </button>
        </div>

        <form onSubmit={handleSubmit}>
          <div className="mb-6">
            <label className="block text-sm font-medium text-gray-700 mb-2">Select Contact</label>
            <ContactSelector
              contacts={contactsData?.contacts || []}
              value={selectedContactId}
              onChange={setSelectedContactId}
              placeholder="Search for a contact..."
              disabled={loading}
              showNoContactOption={false}
            />
          </div>

          <div className="flex justify-end space-x-3">
            <Button type="button" variant="outline" onClick={onCancel} disabled={loading}>
              Cancel
            </Button>
            <Button type="submit" disabled={!selectedContactId} loading={loading}>
              Link Contact
            </Button>
          </div>
        </form>
      </div>
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
  const displayName =
    candidate.display_name ||
    [candidate.first_name, candidate.last_name].filter(Boolean).join(' ') ||
    'Unknown'

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
            <h3 className="text-base font-medium text-gray-900">{displayName}</h3>

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
                  className="inline-flex items-center px-2 py-0.5 rounded bg-gray-100 text-sm text-gray-700 hover:bg-blue-50 hover:text-blue-600 transition-colors"
                >
                  <Mail className="w-3.5 h-3.5 mr-1.5 text-gray-400" />
                  {email}
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
            </div>
          </div>
        </div>

        {/* Right side: Actions */}
        <div className="flex items-center space-x-2 ml-4">
          <Button size="sm" onClick={onImport} loading={importLoading} disabled={ignoreLoading}>
            <UserPlus className="w-4 h-4 mr-1" />
            Import
          </Button>
          <Button
            size="sm"
            variant="outline"
            onClick={onLink}
            disabled={importLoading || ignoreLoading}
          >
            <Link2 className="w-4 h-4 mr-1" />
            Link
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
  const [params, setParams] = useState<ImportCandidatesListParams>({
    page: 1,
    limit: DEFAULT_PAGE_SIZE,
  })
  const [notification, setNotification] = useState<{
    type: 'success' | 'error'
    message: string
  } | null>(null)
  const [linkModalCandidate, setLinkModalCandidate] = useState<ImportCandidate | null>(null)
  const [actionInProgress, setActionInProgress] = useState<string | null>(null)

  const { data, isLoading, error } = useImportCandidates(params)
  const { data: googleAccounts } = useGoogleAccounts()
  const importMutation = useImportAsContact()
  const linkMutation = useLinkCandidate()
  const ignoreMutation = useIgnoreCandidate()
  const syncMutation = useTriggerSync()

  const handleImport = async (candidate: ImportCandidate) => {
    const displayName =
      candidate.display_name ||
      [candidate.first_name, candidate.last_name].filter(Boolean).join(' ') ||
      'contact'

    setActionInProgress(candidate.id)
    try {
      await importMutation.mutateAsync(candidate.id)
      setNotification({
        type: 'success',
        message: `${displayName} imported successfully!`,
      })
    } catch (error) {
      const errorMessage =
        error instanceof Error ? error.message : `Failed to import ${displayName}`
      setNotification({
        type: 'error',
        message: errorMessage,
      })
    } finally {
      setActionInProgress(null)
    }
  }

  const handleLink = async (candidateId: string, crmContactId: string) => {
    setActionInProgress(candidateId)
    try {
      await linkMutation.mutateAsync({ id: candidateId, crmContactId })
      setNotification({
        type: 'success',
        message: 'Contact linked successfully!',
      })
      setLinkModalCandidate(null)
    } catch (error) {
      const errorMessage = error instanceof Error ? error.message : 'Failed to link contact'
      setNotification({
        type: 'error',
        message: errorMessage,
      })
    } finally {
      setActionInProgress(null)
    }
  }

  const handleIgnore = async (candidate: ImportCandidate) => {
    const displayName =
      candidate.display_name ||
      [candidate.first_name, candidate.last_name].filter(Boolean).join(' ') ||
      'contact'

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

  const handleSync = async () => {
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
        message: `Sync started for ${googleAccounts.length} account${googleAccounts.length > 1 ? 's' : ''}! New contacts will appear shortly.`,
      })
    } catch (error) {
      // Extract error message from API response
      let errorMessage = 'Failed to start sync. Please try again.'
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

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-5xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="md:flex md:items-center md:justify-between mb-6">
          <div className="flex-1 min-w-0">
            <div className="flex items-center space-x-3">
              <CloudDownload className="w-8 h-8 text-blue-600" />
              <h2 className="text-2xl font-bold leading-7 text-gray-900 sm:text-3xl sm:truncate">
                Import Contacts
              </h2>
            </div>
            <p className="mt-2 text-sm text-gray-500">
              {isLoading
                ? 'Loading...'
                : data?.total
                  ? `${data.total} contacts available to import from Google`
                  : 'No contacts to import'}
            </p>
          </div>
          <div className="mt-4 flex md:mt-0 md:ml-4">
            <Button variant="outline" onClick={handleSync} loading={syncMutation.isPending}>
              <RefreshCw className="w-4 h-4 mr-2" />
              Sync Google Contacts
            </Button>
          </div>
        </div>

        {/* Notification */}
        {notification && (
          <Notification
            type={notification.type}
            message={notification.message}
            onDismiss={() => setNotification(null)}
          />
        )}

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
        {!isLoading && !error && data?.candidates.length === 0 && (
          <div className="text-center py-12 bg-white rounded-lg border border-gray-200">
            <CloudDownload className="mx-auto h-12 w-12 text-gray-400" />
            <h3 className="mt-2 text-sm font-medium text-gray-900">No import candidates</h3>
            <p className="mt-1 text-sm text-gray-500">
              All contacts from Google have been imported or are already linked.
            </p>
            <div className="mt-6">
              <Button variant="outline" onClick={handleSync} loading={syncMutation.isPending}>
                <RefreshCw className="w-4 h-4 mr-2" />
                Sync Google Contacts
              </Button>
            </div>
          </div>
        )}

        {/* Candidates list */}
        {!isLoading && !error && data && data.candidates.length > 0 && (
          <div className="space-y-3">
            {data.candidates.map(candidate => (
              <CandidateCard
                key={candidate.id}
                candidate={candidate}
                onImport={() => handleImport(candidate)}
                onLink={() => setLinkModalCandidate(candidate)}
                onIgnore={() => handleIgnore(candidate)}
                importLoading={actionInProgress === candidate.id && importMutation.isPending}
                ignoreLoading={actionInProgress === candidate.id && ignoreMutation.isPending}
              />
            ))}
          </div>
        )}

        {/* Pagination */}
        {data && data.pages > 1 && (
          <div className="mt-6 flex items-center justify-between">
            <div className="text-sm text-gray-700">
              Page {data.page} of {data.pages} ({data.total} total)
            </div>
            <div className="flex space-x-2">
              <Button
                variant="outline"
                size="sm"
                disabled={data.page <= 1}
                onClick={() => setParams(prev => ({ ...prev, page: (prev.page || 1) - 1 }))}
              >
                Previous
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={data.page >= data.pages}
                onClick={() => setParams(prev => ({ ...prev, page: (prev.page || 1) + 1 }))}
              >
                Next
              </Button>
            </div>
          </div>
        )}
      </div>

      {/* Link modal */}
      {linkModalCandidate && (
        <LinkContactModal
          candidate={linkModalCandidate}
          onLink={contactId => handleLink(linkModalCandidate.id, contactId)}
          onCancel={() => setLinkModalCandidate(null)}
          loading={linkMutation.isPending}
        />
      )}
    </div>
  )
}
