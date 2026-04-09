'use client'

import { useState, useEffect, useCallback } from 'react'
import { Send, CheckCircle, AlertCircle, Info, LogOut, Loader2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  useTelegramStatus,
  useStartTelegramAuth,
  useVerifyTelegramCode,
  useVerifyTelegramPassword,
  useDisconnectTelegram,
  useTelegramChats,
  useUpdateTelegramChatStatus,
} from '@/hooks/use-telegram'
import { telegramApi } from '@/lib/telegram-api'

type Step =
  | 'loading'
  | 'not_configured'
  | 'fetch_error'
  | 'disconnected'
  | 'phone_input'
  | 'awaiting_code'
  | 'awaiting_password'
  | 'connected'

export function TelegramSection() {
  const { data: status, error: statusError, isLoading } = useTelegramStatus()
  const startAuth = useStartTelegramAuth()
  const verifyCode = useVerifyTelegramCode()
  const verifyPassword = useVerifyTelegramPassword()
  const disconnect = useDisconnectTelegram()

  const [step, setStep] = useState<Step>('loading')
  const [phone, setPhone] = useState('')
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const [authToken, setAuthToken] = useState('')
  const [codeType, setCodeType] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [notification, setNotification] = useState<{
    type: 'success' | 'error'
    message: string
  } | null>(null)

  // Determine step from status query
  useEffect(() => {
    if (isLoading) return

    if (statusError) {
      // 404 means routes not registered (Telegram not configured on backend)
      const statusCode =
        statusError instanceof Error && 'status' in statusError
          ? (statusError as { status: number }).status
          : undefined
      if (statusCode === 404) {
        setStep('not_configured')
      } else {
        setStep('fetch_error')
      }
      return
    }

    if (status?.connected) {
      setStep('connected')
    } else if (step === 'loading' || step === 'fetch_error') {
      setStep('disconnected')
    }
  }, [status, statusError, isLoading, step])

  // Auto-dismiss notifications
  useEffect(() => {
    if (notification) {
      const timer = setTimeout(() => setNotification(null), 5000)
      return () => clearTimeout(timer)
    }
  }, [notification])

  const handleStartAuth = useCallback(async () => {
    setError(null)
    try {
      const result = await startAuth.mutateAsync({ phone_number: phone })
      setAuthToken(result.auth_token)
      setCodeType(result.code_type)
      setStep('awaiting_code')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to start auth')
    }
  }, [phone, startAuth])

  const handleVerifyCode = useCallback(async () => {
    setError(null)
    try {
      const result = await verifyCode.mutateAsync({ auth_token: authToken, code })
      if (result.status === 'connected') {
        setStep('connected')
        setNotification({ type: 'success', message: `Connected as @${result.username}` })
        resetAuthState()
      } else if (result.status === 'awaiting_password') {
        setStep('awaiting_password')
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Invalid code')
    }
  }, [authToken, code, verifyCode])

  const handleVerifyPassword = useCallback(async () => {
    setError(null)
    try {
      const result = await verifyPassword.mutateAsync({ auth_token: authToken, password })
      if (result.status === 'connected') {
        setStep('connected')
        setNotification({ type: 'success', message: `Connected as @${result.username}` })
        resetAuthState()
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Invalid password')
    }
  }, [authToken, password, verifyPassword])

  const handleDisconnect = useCallback(async () => {
    if (!confirm('Disconnect Telegram? This will stop syncing messages.')) return
    try {
      await disconnect.mutateAsync()
      setStep('disconnected')
      setNotification({ type: 'success', message: 'Telegram disconnected' })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to disconnect')
    }
  }, [disconnect])

  const resetAuthState = () => {
    setPhone('')
    setCode('')
    setPassword('')
    setAuthToken('')
    setCodeType('')
    setError(null)
  }

  const handleCancel = async () => {
    // Cancel the server-side auth session so StartAuth can be called again
    try {
      await telegramApi.cancelAuth()
    } catch {
      // Best effort — server may not have an active session
    }
    resetAuthState()
    setStep('disconnected')
  }

  return (
    <section className="bg-white rounded-lg shadow-sm border p-6">
      {/* Header */}
      <div className="flex items-center space-x-3 mb-2">
        <Send className="w-6 h-6 text-blue-500" />
        <h2 className="text-xl font-semibold text-gray-900">Telegram</h2>
      </div>
      <p className="text-gray-600 text-sm mb-4">
        Sync private message interactions from your Telegram account.
      </p>

      {/* Notification */}
      {notification && (
        <div
          className={`flex items-center gap-2 p-3 rounded-lg mb-4 ${
            notification.type === 'success'
              ? 'bg-green-50 text-green-700 border border-green-100'
              : 'bg-red-50 text-red-700 border border-red-100'
          }`}
        >
          {notification.type === 'success' ? (
            <CheckCircle className="w-4 h-4 flex-shrink-0" />
          ) : (
            <AlertCircle className="w-4 h-4 flex-shrink-0" />
          )}
          <span className="text-sm">{notification.message}</span>
        </div>
      )}

      {/* Error */}
      {error && (
        <div className="flex items-center gap-2 p-3 rounded-lg mb-4 bg-red-50 text-red-700 border border-red-100">
          <AlertCircle className="w-4 h-4 flex-shrink-0" />
          <span className="text-sm">{error}</span>
        </div>
      )}

      {/* Loading */}
      {step === 'loading' && (
        <div className="flex items-center justify-center py-8">
          <Loader2 className="w-6 h-6 animate-spin text-gray-400" />
        </div>
      )}

      {/* Not configured */}
      {step === 'not_configured' && (
        <div className="p-4 bg-gray-50 border border-gray-200 rounded-lg">
          <div className="flex items-start gap-2">
            <Info className="w-5 h-5 text-gray-400 mt-0.5 flex-shrink-0" />
            <div>
              <p className="text-sm font-medium text-gray-700">Configuration Required</p>
              <p className="text-sm text-gray-500 mt-1">
                Set the following environment variables to enable Telegram integration:
              </p>
              <ul className="text-sm text-gray-500 mt-2 list-disc list-inside space-y-1">
                <li>
                  <code className="text-xs bg-gray-100 px-1 rounded">
                    ENABLE_TELEGRAM_SYNC=true
                  </code>
                </li>
                <li>
                  <code className="text-xs bg-gray-100 px-1 rounded">TELEGRAM_API_ID</code>
                </li>
                <li>
                  <code className="text-xs bg-gray-100 px-1 rounded">TELEGRAM_API_HASH</code>
                </li>
              </ul>
              <p className="text-sm text-gray-500 mt-2">
                Get API credentials from <span className="font-medium">my.telegram.org/apps</span>.
              </p>
            </div>
          </div>
        </div>
      )}

      {/* Fetch error */}
      {step === 'fetch_error' && (
        <div className="text-center py-6">
          <AlertCircle className="w-8 h-8 text-red-400 mx-auto mb-2" />
          <p className="text-sm text-gray-600 mb-3">Failed to load Telegram status</p>
          <Button variant="outline" size="sm" onClick={() => window.location.reload()}>
            Retry
          </Button>
        </div>
      )}

      {/* Disconnected */}
      {step === 'disconnected' && (
        <div className="text-center py-6">
          <p className="text-sm text-gray-500 mb-4">
            Connect your Telegram account to automatically track message interactions.
          </p>
          <Button onClick={() => setStep('phone_input')}>Connect Telegram</Button>
        </div>
      )}

      {/* Phone input */}
      {step === 'phone_input' && (
        <div className="space-y-4">
          <div>
            <label htmlFor="tg-phone" className="block text-sm font-medium text-gray-700 mb-1">
              Phone Number
            </label>
            <input
              id="tg-phone"
              type="tel"
              className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm text-gray-900 placeholder-gray-400 focus:outline-none focus:ring-2 focus:ring-blue-500"
              placeholder="+1234567890"
              value={phone}
              onChange={e => setPhone(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleStartAuth()}
            />
            <p className="text-xs text-gray-400 mt-1">International format with country code</p>
          </div>
          <div className="flex gap-2">
            <Button
              onClick={handleStartAuth}
              loading={startAuth.isPending}
              disabled={!phone.trim()}
            >
              Send Code
            </Button>
            <Button variant="outline" onClick={handleCancel}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {/* Awaiting code */}
      {step === 'awaiting_code' && (
        <div className="space-y-4">
          <p className="text-sm text-gray-600">
            {codeType === 'app'
              ? 'A code was sent to your Telegram app.'
              : codeType === 'sms'
                ? 'A code was sent via SMS.'
                : 'A verification code was sent.'}
          </p>
          <div>
            <label htmlFor="tg-code" className="block text-sm font-medium text-gray-700 mb-1">
              Verification Code
            </label>
            <input
              id="tg-code"
              type="text"
              inputMode="numeric"
              className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm text-gray-900 placeholder-gray-400 focus:outline-none focus:ring-2 focus:ring-blue-500"
              placeholder="12345"
              value={code}
              onChange={e => setCode(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleVerifyCode()}
              autoFocus
            />
          </div>
          <div className="flex gap-2">
            <Button
              onClick={handleVerifyCode}
              loading={verifyCode.isPending}
              disabled={!code.trim()}
            >
              Verify
            </Button>
            <Button variant="outline" onClick={handleCancel}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {/* Awaiting password (2FA) */}
      {step === 'awaiting_password' && (
        <div className="space-y-4">
          <p className="text-sm text-gray-600">
            Your account has two-factor authentication enabled. Please enter your password.
          </p>
          <div>
            <label htmlFor="tg-password" className="block text-sm font-medium text-gray-700 mb-1">
              2FA Password
            </label>
            <input
              id="tg-password"
              type="password"
              className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm text-gray-900 placeholder-gray-400 focus:outline-none focus:ring-2 focus:ring-blue-500"
              placeholder="Your 2FA password"
              value={password}
              onChange={e => setPassword(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleVerifyPassword()}
              autoFocus
            />
          </div>
          <div className="flex gap-2">
            <Button
              onClick={handleVerifyPassword}
              loading={verifyPassword.isPending}
              disabled={!password.trim()}
            >
              Verify
            </Button>
            <Button variant="outline" onClick={handleCancel}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {/* Connected */}
      {step === 'connected' && status && (
        <div className="space-y-4">
          <div className="flex items-center gap-2">
            <CheckCircle className="w-5 h-5 text-green-500" />
            <span className="text-sm font-medium text-gray-900">
              Connected{status.username ? ` as @${status.username}` : ''}
            </span>
          </div>
          {status.phone_number && (
            <p className="text-sm text-gray-500">Phone: {status.phone_number}</p>
          )}
          <Button
            variant="outline"
            size="sm"
            onClick={handleDisconnect}
            loading={disconnect.isPending}
            className="text-red-600 hover:text-red-700"
          >
            <LogOut className="w-4 h-4 mr-1" />
            Disconnect
          </Button>

          {/* Backfill progress */}
          {status.backfill_in_progress && (
            <div className="mt-4 p-3 bg-blue-50 border border-blue-100 rounded-lg">
              <div className="flex items-center gap-2 mb-2">
                <Loader2 className="w-4 h-4 animate-spin text-blue-500" />
                <span className="text-sm font-medium text-blue-700">
                  Syncing messages... {status.backfill_completed}/{status.backfill_total} chats
                </span>
              </div>
              {status.backfill_total && status.backfill_total > 0 && (
                <div className="w-full bg-blue-200 rounded-full h-1.5">
                  <div
                    className="bg-blue-500 h-1.5 rounded-full transition-all"
                    style={{
                      width: `${((status.backfill_completed ?? 0) / status.backfill_total) * 100}%`,
                    }}
                  />
                </div>
              )}
            </div>
          )}

          {/* Group chat management */}
          <TelegramChatList />
        </div>
      )}

      {/* Configuration info box */}
      <div className="mt-6 p-4 bg-blue-50 border border-blue-100 rounded-lg">
        <div className="flex items-start gap-2">
          <Info className="w-4 h-4 text-blue-500 mt-0.5 flex-shrink-0" />
          <div className="text-sm text-blue-700">
            <p className="font-medium">About Telegram Integration</p>
            <p className="mt-1 text-blue-600">
              This uses the Telegram user API (not a bot) to sync private message interactions. Your
              messages are processed locally and never sent to external services.
            </p>
          </div>
        </div>
      </div>
    </section>
  )
}

function TelegramChatList() {
  const { data: chats, isLoading, error } = useTelegramChats()
  const updateStatus = useUpdateTelegramChatStatus()

  if (isLoading) {
    return (
      <div className="mt-4 flex items-center justify-center py-4">
        <Loader2 className="w-5 h-5 animate-spin text-gray-400" />
      </div>
    )
  }

  if (error) {
    return (
      <div className="mt-4 p-3 bg-red-50 border border-red-100 rounded-lg text-sm text-red-600">
        Failed to load chat list
      </div>
    )
  }

  if (!chats || chats.length === 0) {
    return (
      <div className="mt-4 p-4 border-2 border-dashed border-gray-200 rounded-lg bg-gray-50 text-center">
        <p className="text-sm text-gray-500">
          No group chats discovered yet. Chats will appear here after the initial sync completes.
        </p>
      </div>
    )
  }

  return (
    <div className="mt-4 space-y-1">
      <h3 className="text-sm font-medium text-gray-700 mb-2">Group Chats</h3>
      {updateStatus.isError && (
        <div className="flex items-center gap-2 p-2 rounded bg-red-50 text-red-600 text-sm mb-2">
          <AlertCircle className="w-4 h-4 flex-shrink-0" />
          Failed to update chat status
        </div>
      )}
      {chats.map(chat => (
        <div
          key={chat.telegram_chat_id}
          className="flex items-center justify-between p-3 bg-gray-50 rounded-lg"
        >
          <div className="flex items-center gap-2 min-w-0">
            <span
              className={`w-2 h-2 rounded-full flex-shrink-0 ${
                chat.effective_tracked ? 'bg-green-500' : 'bg-gray-300'
              }`}
            />
            <span className="text-sm text-gray-900 truncate">{chat.chat_title || 'Untitled'}</span>
            {chat.member_count != null && (
              <span className="text-xs text-gray-400 flex-shrink-0">
                {chat.member_count} members
              </span>
            )}
          </div>
          <select
            value={chat.status}
            onChange={e =>
              updateStatus.mutate({
                chatId: chat.telegram_chat_id,
                status: e.target.value as 'auto' | 'ignored' | 'tracked',
              })
            }
            className="text-sm text-gray-700 bg-white border border-gray-300 rounded px-2 py-1 focus:outline-none focus:ring-2 focus:ring-blue-500"
          >
            <option value="auto">Auto</option>
            <option value="ignored">Ignored</option>
            <option value="tracked">Tracked</option>
          </select>
        </div>
      ))}
    </div>
  )
}
