'use client'

import { useEffect, useState } from 'react'
import { useSearchParams } from 'next/navigation'
import {
  Mail,
  MessageSquare,
  Plus,
  Trash2,
  CheckCircle,
  AlertCircle,
  Info,
  RefreshCcw,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { SyncBadge } from '@/components/settings/sync-badge'
import { useGoogleAccounts, useRevokeGoogleAccount } from '@/hooks/use-google-accounts'
import {
  useSyncStates,
  getSyncStateForAccount,
  accountNeedsReconnection,
} from '@/hooks/use-sync-states'
import { useTriggerSync } from '@/hooks/use-imports'
import { startGoogleOAuthFlow, GoogleAccount } from '@/lib/oauth-api'

// GCHAT_REQUIRED_SCOPES gates the Google Chat sync badge. It mirrors the backend
// enablement gate (service.gchatRequiredScopes): the sweep needs all three Chat
// scopes (spaces.list, messages.list, members.list), so the badge only shows
// once the account has re-consented with ALL of them — a partial grant shows the
// reconnect hint instead. Like the other Google badges, this is the
// requested-scope proxy (the stored scope list), not a true-grant check.
const GCHAT_REQUIRED_SCOPES = [
  'https://www.googleapis.com/auth/chat.spaces.readonly',
  'https://www.googleapis.com/auth/chat.messages.readonly',
  'https://www.googleapis.com/auth/chat.memberships.readonly',
]

/**
 * accountHasChatScopes reports whether a connected Google account carries ALL
 * THREE chat scopes — the gate for the Google Chat sync badge. A partial grant
 * returns false (the reconnect hint is shown instead). Exported as a pure
 * function so it can be unit-tested without rendering.
 */
export function accountHasChatScopes(account: Pick<GoogleAccount, 'scopes'>): boolean {
  const scopes = account.scopes
  if (!scopes) return false
  return GCHAT_REQUIRED_SCOPES.every(required => scopes.includes(required))
}

function formatDate(dateString: string): string {
  return new Date(dateString).toLocaleDateString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
  })
}

export function GoogleAccountsSection() {
  const searchParams = useSearchParams()
  const { data: accounts, isLoading, error, refetch } = useGoogleAccounts()
  const { data: syncStates } = useSyncStates()
  const revokeMutation = useRevokeGoogleAccount()
  const triggerSyncMutation = useTriggerSync()
  const [isConnecting, setIsConnecting] = useState(false)
  const [syncingAccount, setSyncingAccount] = useState<string | null>(null)
  const [notification, setNotification] = useState<{
    type: 'success' | 'error'
    message: string
  } | null>(null)

  const handleSyncCalendar = async (accountId: string) => {
    setSyncingAccount(`gcal-${accountId}`)
    try {
      await triggerSyncMutation.mutateAsync({ source: 'gcal', accountId })
      setNotification({
        type: 'success',
        message: 'Calendar sync started!',
      })
    } catch (error) {
      console.error('Calendar sync error:', error)
      const errorMessage = error instanceof Error ? error.message : 'Unknown error'
      setNotification({
        type: 'error',
        message: errorMessage.includes('authentication')
          ? 'Authentication failed. Please reconnect your account.'
          : `Failed to start calendar sync: ${errorMessage}`,
      })
    } finally {
      setSyncingAccount(null)
    }
  }

  const handleSyncContacts = async (accountId: string) => {
    setSyncingAccount(`gcontacts-${accountId}`)
    try {
      await triggerSyncMutation.mutateAsync({ source: 'gcontacts', accountId })
      setNotification({
        type: 'success',
        message: 'Contacts sync started!',
      })
    } catch (error) {
      console.error('Contacts sync error:', error)
      const errorMessage = error instanceof Error ? error.message : 'Unknown error'
      setNotification({
        type: 'error',
        message: errorMessage.includes('authentication')
          ? 'Authentication failed. Please reconnect your account.'
          : `Failed to start contacts sync: ${errorMessage}`,
      })
    } finally {
      setSyncingAccount(null)
    }
  }

  const handleSyncEmail = async (accountId: string) => {
    setSyncingAccount(`email-${accountId}`)
    try {
      await triggerSyncMutation.mutateAsync({ source: 'email', accountId })
      setNotification({
        type: 'success',
        message: 'Gmail sync started!',
      })
    } catch (error) {
      console.error('Gmail sync error:', error)
      const errorMessage = error instanceof Error ? error.message : 'Unknown error'
      setNotification({
        type: 'error',
        message: errorMessage.includes('authentication')
          ? 'Authentication failed. Please reconnect your account.'
          : `Failed to start Gmail sync: ${errorMessage}`,
      })
    } finally {
      setSyncingAccount(null)
    }
  }

  const handleSyncGChat = async (accountId: string) => {
    setSyncingAccount(`gchat-${accountId}`)
    try {
      await triggerSyncMutation.mutateAsync({ source: 'gchat', accountId })
      setNotification({
        type: 'success',
        message: 'Google Chat sync started!',
      })
    } catch (error) {
      console.error('Google Chat sync error:', error)
      const errorMessage = error instanceof Error ? error.message : 'Unknown error'
      setNotification({
        type: 'error',
        message: errorMessage.includes('authentication')
          ? 'Authentication failed. Please reconnect your account.'
          : `Failed to start Google Chat sync: ${errorMessage}`,
      })
    } finally {
      setSyncingAccount(null)
    }
  }

  // Handle OAuth callback query params
  useEffect(() => {
    const auth = searchParams.get('auth')
    const provider = searchParams.get('provider')
    const message = searchParams.get('message')

    if (auth && provider === 'google') {
      if (auth === 'success') {
        setNotification({
          type: 'success',
          message: 'Google account connected successfully!',
        })
        refetch()
      } else if (auth === 'error') {
        setNotification({
          type: 'error',
          message: message
            ? `Failed to connect: ${message.replace(/_/g, ' ')}`
            : 'Failed to connect Google account.',
        })
      }

      // Clear the query params after showing notification
      const timeout = setTimeout(() => {
        window.history.replaceState({}, '', '/settings')
      }, 500)

      return () => clearTimeout(timeout)
    }
  }, [searchParams, refetch])

  // Auto-dismiss notifications
  useEffect(() => {
    if (notification) {
      const timeout = setTimeout(() => setNotification(null), 5000)
      return () => clearTimeout(timeout)
    }
  }, [notification])

  const handleConnectGoogle = async () => {
    setIsConnecting(true)
    try {
      await startGoogleOAuthFlow()
    } catch {
      setNotification({
        type: 'error',
        message: 'Failed to start Google authorization. Please try again.',
      })
      setIsConnecting(false)
    }
  }

  const handleDisconnect = async (account: GoogleAccount) => {
    if (
      !confirm(
        `Disconnect ${account.account_id}? This will revoke access to Gmail, Calendar, and Contacts for this account.`
      )
    ) {
      return
    }

    try {
      await revokeMutation.mutateAsync(account.id)
      setNotification({
        type: 'success',
        message: `Disconnected ${account.account_id}`,
      })
    } catch {
      setNotification({
        type: 'error',
        message: 'Failed to disconnect account. Please try again.',
      })
    }
  }

  // Show empty state when no accounts or when there's an error (feature not configured)
  const showEmptyState = !isLoading && (error || accounts?.length === 0)
  const hasAccounts = !isLoading && !error && accounts && accounts.length > 0

  return (
    <section className="bg-white rounded-lg shadow-sm border p-6">
      {/* Header */}
      <div className="flex items-center justify-between mb-4">
        <div className="flex items-center space-x-3">
          <Mail className="w-6 h-6 text-red-500" />
          <h2 className="text-xl font-semibold text-gray-900">Google Accounts</h2>
        </div>
        {hasAccounts && (
          <Button
            onClick={handleConnectGoogle}
            loading={isConnecting}
            size="sm"
            className="flex items-center space-x-1"
          >
            <Plus className="w-4 h-4" />
            <span>Add Account</span>
          </Button>
        )}
      </div>

      {/* Description */}
      <p className="text-gray-600 mb-6">
        Connect your Google accounts to sync emails, calendar events, and contacts. You can connect
        multiple accounts (e.g., personal and work).
      </p>

      {/* Notification */}
      {notification && (
        <div
          className={`mb-6 p-4 rounded-lg flex items-start space-x-3 ${
            notification.type === 'success'
              ? 'bg-green-50 border border-green-200'
              : 'bg-red-50 border border-red-200'
          }`}
        >
          {notification.type === 'success' ? (
            <CheckCircle className="w-5 h-5 text-green-600 flex-shrink-0 mt-0.5" />
          ) : (
            <AlertCircle className="w-5 h-5 text-red-600 flex-shrink-0 mt-0.5" />
          )}
          <p
            className={`text-sm ${
              notification.type === 'success' ? 'text-green-800' : 'text-red-800'
            }`}
          >
            {notification.message}
          </p>
        </div>
      )}

      {/* Loading state */}
      {isLoading && (
        <div className="py-12 text-center">
          <div className="animate-spin inline-block w-8 h-8 border-2 border-gray-200 border-t-blue-600 rounded-full mb-3" />
          <p className="text-gray-500">Loading accounts...</p>
        </div>
      )}

      {/* Empty state */}
      {showEmptyState && (
        <div className="py-12 text-center border-2 border-dashed border-gray-200 rounded-lg bg-gray-50">
          <div className="w-16 h-16 mx-auto mb-4 rounded-full bg-gray-100 flex items-center justify-center">
            <Mail className="w-8 h-8 text-gray-400" />
          </div>
          <h3 className="text-lg font-medium text-gray-900 mb-2">No Google accounts connected</h3>
          <p className="text-gray-500 mb-6 max-w-sm mx-auto">
            Connect a Google account to start syncing your emails, calendar, and contacts.
          </p>
          <Button onClick={handleConnectGoogle} loading={isConnecting}>
            <Plus className="w-4 h-4 mr-2" />
            Connect Google Account
          </Button>
        </div>
      )}

      {/* Accounts list */}
      {hasAccounts && (
        <div className="space-y-4">
          {accounts.map(account => (
            <div key={account.id} className="p-4 rounded-lg border bg-gray-50 border-gray-200">
              <div className="flex items-start justify-between">
                <div className="flex-1 min-w-0">
                  <div className="flex items-center space-x-2 mb-1">
                    <p className="font-medium text-gray-900 truncate">{account.account_id}</p>
                    {account.created_at && (
                      <span className="text-xs text-gray-500">
                        Connected {formatDate(account.created_at)}
                      </span>
                    )}
                  </div>
                  {account.account_name && (
                    <p className="text-sm text-gray-600">{account.account_name}</p>
                  )}
                </div>
                <div className="flex items-center space-x-2 -mr-2">
                  {accountNeedsReconnection(syncStates, account.account_id, account.updated_at) && (
                    <Button
                      onClick={handleConnectGoogle}
                      loading={isConnecting}
                      variant="outline"
                      size="sm"
                      className="text-amber-600 border-amber-300 hover:bg-amber-50 hover:border-amber-400"
                    >
                      <RefreshCcw className="w-4 h-4 mr-1" />
                      Reconnect
                    </Button>
                  )}
                  <Button
                    onClick={() => handleDisconnect(account)}
                    loading={revokeMutation.isPending}
                    variant="ghost"
                    size="sm"
                    className="text-red-600 hover:text-red-700 hover:bg-red-50"
                  >
                    <Trash2 className="w-4 h-4" />
                  </Button>
                </div>
              </div>

              {/* Permissions & Sync (Option B: Compact Badges) */}
              <div className="mt-4 pt-4 border-t border-gray-200">
                <p className="text-xs font-medium text-gray-500 uppercase tracking-wide mb-2">
                  Permissions & Sync
                </p>
                <div className="flex flex-wrap gap-2">
                  {/* Gmail - sync badge */}
                  {account.scopes?.includes('https://www.googleapis.com/auth/gmail.readonly') && (
                    <SyncBadge
                      label="Gmail"
                      syncState={getSyncStateForAccount(syncStates, 'email', account.account_id)}
                      onSync={() => handleSyncEmail(account.account_id)}
                      loading={syncingAccount === `email-${account.account_id}`}
                    />
                  )}
                  {/* Calendar - sync badge */}
                  {account.scopes?.includes(
                    'https://www.googleapis.com/auth/calendar.readonly'
                  ) && (
                    <SyncBadge
                      label="Calendar"
                      syncState={getSyncStateForAccount(syncStates, 'gcal', account.account_id)}
                      onSync={() => handleSyncCalendar(account.account_id)}
                      loading={syncingAccount === `gcal-${account.account_id}`}
                    />
                  )}
                  {/* Contacts - sync badge */}
                  {account.scopes?.includes(
                    'https://www.googleapis.com/auth/contacts.readonly'
                  ) && (
                    <SyncBadge
                      label="Contacts"
                      syncState={getSyncStateForAccount(
                        syncStates,
                        'gcontacts',
                        account.account_id
                      )}
                      onSync={() => handleSyncContacts(account.account_id)}
                      loading={syncingAccount === `gcontacts-${account.account_id}`}
                    />
                  )}
                  {/* Google Chat - sync badge (scoped) or reconnect hint (unscoped) */}
                  {accountHasChatScopes(account) ? (
                    <SyncBadge
                      label="Chat"
                      syncState={getSyncStateForAccount(syncStates, 'gchat', account.account_id)}
                      onSync={() => handleSyncGChat(account.account_id)}
                      loading={syncingAccount === `gchat-${account.account_id}`}
                    />
                  ) : (
                    <button
                      type="button"
                      onClick={handleConnectGoogle}
                      disabled={isConnecting}
                      title="Reconnect this account to grant Google Chat access"
                      className="inline-flex items-center gap-1 px-2.5 py-1 rounded-md text-xs font-medium bg-amber-50 border border-amber-300 text-amber-700 hover:bg-amber-100 disabled:opacity-50"
                    >
                      <MessageSquare className="w-3 h-3" />
                      Chat — reconnect required
                    </button>
                  )}
                </div>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* Setup instructions - always shown at bottom */}
      <div className="mt-6 p-5 bg-blue-50 border border-blue-100 rounded-lg">
        <div className="flex items-start space-x-3">
          <Info className="w-5 h-5 text-blue-600 flex-shrink-0 mt-0.5" />
          <div>
            <h4 className="font-medium text-blue-900 mb-2">Configuration Required</h4>
            <p className="text-sm text-blue-800 leading-relaxed mb-3">
              To use Google integration, configure your Google Cloud OAuth credentials:
            </p>
            <div className="space-y-2">
              <div className="flex items-center space-x-2">
                <code className="px-2 py-1 bg-blue-100 rounded text-xs font-mono text-blue-900">
                  GOOGLE_CLIENT_ID
                </code>
                <span className="text-xs text-blue-700">Your OAuth 2.0 Client ID</span>
              </div>
              <div className="flex items-center space-x-2">
                <code className="px-2 py-1 bg-blue-100 rounded text-xs font-mono text-blue-900">
                  GOOGLE_CLIENT_SECRET
                </code>
                <span className="text-xs text-blue-700">Your OAuth 2.0 Client Secret</span>
              </div>
              <div className="flex items-center space-x-2">
                <code className="px-2 py-1 bg-blue-100 rounded text-xs font-mono text-blue-900">
                  TOKEN_ENCRYPTION_KEY
                </code>
                <span className="text-xs text-blue-700">
                  32-byte hex key (<code className="font-mono">openssl rand -hex 32</code>)
                </span>
              </div>
            </div>
          </div>
        </div>
      </div>
    </section>
  )
}
