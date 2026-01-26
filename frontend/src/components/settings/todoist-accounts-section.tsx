'use client'

import { useEffect, useState } from 'react'
import { useSearchParams } from 'next/navigation'
import {
  CheckSquare,
  Plus,
  Trash2,
  CheckCircle,
  AlertCircle,
  Info,
  RefreshCcw,
  RefreshCw,
  Settings,
  ChevronDown,
} from 'lucide-react'
import { Button } from '@/components/ui/button'
import { useTodoistAccounts, useRevokeTodoistAccount } from '@/hooks/use-todoist-accounts'
import {
  useTodoistSettings,
  useUpdateTodoistSettings,
  useTodoistProjects,
  useTodoistLabels,
} from '@/hooks/use-todoist-settings'
import {
  useSyncStates,
  accountNeedsReconnection,
  getSyncStateForAccount,
  formatSyncTime,
} from '@/hooks/use-sync-states'
import { useTriggerSync } from '@/hooks/use-imports'
import { startTodoistOAuthFlow, TodoistAccount } from '@/lib/oauth-api'

function formatDate(dateString: string): string {
  return new Date(dateString).toLocaleDateString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
  })
}

export function TodoistAccountsSection() {
  const searchParams = useSearchParams()
  const { data: accounts, isLoading, error, refetch } = useTodoistAccounts()
  const { data: syncStates } = useSyncStates()
  const revokeMutation = useRevokeTodoistAccount()
  const triggerSyncMutation = useTriggerSync()
  const [isConnecting, setIsConnecting] = useState(false)
  const [isSyncing, setIsSyncing] = useState(false)
  const [notification, setNotification] = useState<{
    type: 'success' | 'error'
    message: string
  } | null>(null)

  // Settings state
  const hasAccount = !isLoading && !error && accounts && accounts.length > 0
  const {
    data: settings,
    isLoading: settingsLoading,
    refetch: refetchSettings,
  } = useTodoistSettings(hasAccount)
  const { data: projects, isLoading: projectsLoading } = useTodoistProjects(hasAccount)
  const { data: labels, isLoading: labelsLoading } = useTodoistLabels(hasAccount)
  const updateSettingsMutation = useUpdateTodoistSettings()

  // Local state for selectors
  const [selectedProjectId, setSelectedProjectId] = useState<string>('')
  const [selectedLabelId, setSelectedLabelId] = useState<string>('')

  // Update local state when settings load
  useEffect(() => {
    if (settings) {
      setSelectedProjectId(settings.project_id || '')
      setSelectedLabelId(settings.label_id || '')
    }
  }, [settings])

  // Save settings when selection changes
  const handleProjectChange = async (projectId: string) => {
    setSelectedProjectId(projectId)
    try {
      await updateSettingsMutation.mutateAsync({ project_id: projectId })
      setNotification({
        type: 'success',
        message: 'Project updated successfully',
      })
    } catch {
      setNotification({
        type: 'error',
        message: 'Failed to update project',
      })
    }
  }

  const handleLabelChange = async (labelId: string) => {
    setSelectedLabelId(labelId)
    try {
      await updateSettingsMutation.mutateAsync({ label_id: labelId })
      setNotification({
        type: 'success',
        message: 'Label updated successfully',
      })
    } catch {
      setNotification({
        type: 'error',
        message: 'Failed to update label',
      })
    }
  }

  const handleTriggerSync = async () => {
    if (!accounts?.[0]) return
    setIsSyncing(true)
    try {
      await triggerSyncMutation.mutateAsync({
        source: 'todoist',
        accountId: accounts[0].account_id,
      })
      setNotification({
        type: 'success',
        message: 'Todoist sync started!',
      })
      refetchSettings()
    } catch (err) {
      console.error('Todoist sync error:', err)
      setNotification({
        type: 'error',
        message: 'Failed to start sync',
      })
    } finally {
      setIsSyncing(false)
    }
  }

  // Handle OAuth callback query params
  useEffect(() => {
    const auth = searchParams.get('auth')
    const provider = searchParams.get('provider')
    const message = searchParams.get('message')

    if (auth && provider === 'todoist') {
      if (auth === 'success') {
        setNotification({
          type: 'success',
          message: 'Todoist account connected successfully!',
        })
        refetch()
      } else if (auth === 'error') {
        setNotification({
          type: 'error',
          message: message
            ? `Failed to connect: ${message.replace(/_/g, ' ')}`
            : 'Failed to connect Todoist account.',
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

  const handleConnectTodoist = async () => {
    setIsConnecting(true)
    try {
      await startTodoistOAuthFlow()
    } catch {
      setNotification({
        type: 'error',
        message: 'Failed to start Todoist authorization. Please try again.',
      })
      setIsConnecting(false)
    }
  }

  const handleDisconnect = async (account: TodoistAccount) => {
    if (
      !confirm(
        `Disconnect ${account.account_name || account.account_id}? This will revoke access to Todoist for this account.`
      )
    ) {
      return
    }

    try {
      await revokeMutation.mutateAsync(account.id)
      setNotification({
        type: 'success',
        message: `Disconnected ${account.account_name || account.account_id}`,
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

  // Check if settings are configured
  const isConfigured = settings?.project_id && settings?.label_id

  // Get sync state for status display
  const syncState = hasAccounts
    ? getSyncStateForAccount(syncStates, 'todoist', accounts[0].account_id)
    : undefined
  const lastSyncText = formatSyncTime(syncState?.last_successful_sync_at ?? null)
  const isSyncInProgress = syncState?.status === 'syncing'
  const hasError = syncState?.status === 'error'

  return (
    <section className="bg-white rounded-lg shadow-sm border p-6">
      {/* Header */}
      <div className="flex items-center justify-between mb-4">
        <div className="flex items-center space-x-3">
          <CheckSquare className="w-6 h-6 text-red-500" />
          <h2 className="text-xl font-semibold text-gray-900">Todoist</h2>
        </div>
        {hasAccounts && (
          <Button
            onClick={handleConnectTodoist}
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
        Connect your Todoist account to sync tasks with your CRM contacts.
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
            <CheckSquare className="w-8 h-8 text-gray-400" />
          </div>
          <h3 className="text-lg font-medium text-gray-900 mb-2">No Todoist account connected</h3>
          <p className="text-gray-500 mb-6 max-w-sm mx-auto">
            Connect your Todoist account to start syncing tasks.
          </p>
          <Button onClick={handleConnectTodoist} loading={isConnecting}>
            <Plus className="w-4 h-4 mr-2" />
            Connect Todoist Account
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
                    <p className="font-medium text-gray-900 truncate">
                      {account.account_name || account.account_id}
                    </p>
                    {account.created_at && (
                      <span className="text-xs text-gray-500">
                        Connected {formatDate(account.created_at)}
                      </span>
                    )}
                  </div>
                  {account.account_name && account.account_name !== account.account_id && (
                    <p className="text-sm text-gray-600">ID: {account.account_id}</p>
                  )}
                </div>
                <div className="flex items-center space-x-2 -mr-2">
                  {accountNeedsReconnection(syncStates, account.account_id, account.updated_at) && (
                    <Button
                      onClick={handleConnectTodoist}
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

              {/* Cadence Sync Settings */}
              <div className="mt-4 pt-4 border-t border-gray-200">
                <div className="flex items-center justify-between mb-3">
                  <div className="flex items-center space-x-2">
                    <Settings className="w-4 h-4 text-gray-500" />
                    <p className="text-sm font-medium text-gray-700">Cadence Sync Settings</p>
                  </div>
                  {/* Sync status badge */}
                  <div className="flex items-center space-x-2">
                    <span
                      className={`text-xs px-2 py-1 rounded ${
                        hasError
                          ? 'bg-red-100 text-red-700'
                          : isSyncInProgress
                            ? 'bg-blue-100 text-blue-700'
                            : isConfigured
                              ? 'bg-green-100 text-green-700'
                              : 'bg-gray-100 text-gray-500'
                      }`}
                    >
                      {hasError
                        ? 'Error'
                        : isSyncInProgress
                          ? 'Syncing...'
                          : isConfigured
                            ? `Last: ${lastSyncText}`
                            : 'Not configured'}
                    </span>
                    {isConfigured && (
                      <button
                        onClick={handleTriggerSync}
                        disabled={isSyncing || isSyncInProgress}
                        className="p-1 text-blue-600 hover:bg-blue-50 rounded disabled:opacity-50"
                      >
                        <RefreshCw
                          className={`w-4 h-4 ${isSyncing || isSyncInProgress ? 'animate-spin' : ''}`}
                        />
                      </button>
                    )}
                  </div>
                </div>

                <div className="space-y-3">
                  {/* Project selector */}
                  <div>
                    <label className="block text-xs font-medium text-gray-600 mb-1">
                      Project for cadence tasks
                    </label>
                    <div className="relative">
                      <select
                        value={selectedProjectId}
                        onChange={e => handleProjectChange(e.target.value)}
                        disabled={projectsLoading || updateSettingsMutation.isPending}
                        className="w-full px-3 py-2 text-sm border border-gray-200 rounded-md bg-white appearance-none focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-blue-500 disabled:bg-gray-100 disabled:cursor-not-allowed"
                      >
                        <option value="">Select a project...</option>
                        {projects?.map(project => (
                          <option key={project.id} value={project.id}>
                            {project.name}
                          </option>
                        ))}
                      </select>
                      <ChevronDown className="absolute right-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400 pointer-events-none" />
                    </div>
                  </div>

                  {/* Label selector */}
                  <div>
                    <label className="block text-xs font-medium text-gray-600 mb-1">
                      Label for cadence tasks
                    </label>
                    <div className="relative">
                      <select
                        value={selectedLabelId}
                        onChange={e => handleLabelChange(e.target.value)}
                        disabled={labelsLoading || updateSettingsMutation.isPending}
                        className="w-full px-3 py-2 text-sm border border-gray-200 rounded-md bg-white appearance-none focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-blue-500 disabled:bg-gray-100 disabled:cursor-not-allowed"
                      >
                        <option value="">Select a label...</option>
                        {labels?.map(label => (
                          <option key={label.id} value={label.id}>
                            {label.name}
                          </option>
                        ))}
                      </select>
                      <ChevronDown className="absolute right-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400 pointer-events-none" />
                    </div>
                  </div>

                  {/* Status info */}
                  {!isConfigured && (
                    <p className="text-xs text-amber-600 bg-amber-50 p-2 rounded">
                      Select both a project and label to enable cadence sync
                    </p>
                  )}
                  {isConfigured && (
                    <p className="text-xs text-green-600 bg-green-50 p-2 rounded">
                      Cadence sync is active. Tasks will be created for contacts with cadence set.
                    </p>
                  )}
                </div>
              </div>

              {/* Permissions info */}
              <div className="mt-4 pt-4 border-t border-gray-200">
                <p className="text-xs font-medium text-gray-500 uppercase tracking-wide mb-2">
                  Permissions
                </p>
                <div className="flex flex-wrap gap-2">
                  {account.scopes?.includes('data:read_write') && (
                    <span className="inline-flex items-center px-2.5 py-1 rounded-md text-xs font-medium bg-white border border-gray-200 text-gray-700">
                      Read & Write
                    </span>
                  )}
                  {account.scopes?.includes('data:delete') && (
                    <span className="inline-flex items-center px-2.5 py-1 rounded-md text-xs font-medium bg-white border border-gray-200 text-gray-700">
                      Delete
                    </span>
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
              To use Todoist integration, configure your Todoist OAuth credentials:
            </p>
            <div className="space-y-2">
              <div className="flex items-center space-x-2">
                <code className="px-2 py-1 bg-blue-100 rounded text-xs font-mono text-blue-900">
                  TODOIST_CLIENT_ID
                </code>
                <span className="text-xs text-blue-700">Your OAuth App Client ID</span>
              </div>
              <div className="flex items-center space-x-2">
                <code className="px-2 py-1 bg-blue-100 rounded text-xs font-mono text-blue-900">
                  TODOIST_CLIENT_SECRET
                </code>
                <span className="text-xs text-blue-700">Your OAuth App Client Secret</span>
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
