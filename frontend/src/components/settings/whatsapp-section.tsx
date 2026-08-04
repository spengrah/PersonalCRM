'use client'

import { useCallback, useEffect, useState } from 'react'
import {
  AlertCircle,
  AlertTriangle,
  CheckCircle,
  Info,
  Loader2,
  LogOut,
  MessageCircle,
  QrCode,
  Smartphone,
} from 'lucide-react'
import { QRCodeSVG } from 'qrcode.react'
import { Button } from '@/components/ui/button'
import {
  useCancelWhatsAppPairing,
  useDisconnectWhatsApp,
  useStartWhatsAppPairing,
  useUpdateWhatsAppChatStatus,
  useWhatsAppChats,
  useWhatsAppStatus,
} from '@/hooks/use-whatsapp'
import type { WhatsAppChatStatus, WhatsAppStatus } from '@/lib/whatsapp-api'

export type Step =
  | 'loading'
  | 'not_configured'
  | 'fetch_error'
  | 'not_ready'
  | 'not_paired'
  | 'pairing_qr'
  | 'pairing_phone'
  | 'connecting'
  | 'connected'
  | 'disconnected'
  | 'disconnect_failed'
  | 'error'

/** The one bit of local UI state: which sub-step of not_paired is showing. */
export type PairMode = 'choose' | 'phone' | null

/**
 * deriveWhatsAppStep maps the query result onto a render branch.
 *
 * The step is a pure function of the backend's own state, with pairMode
 * distinguishing the three local sub-steps of linking — "here is a Link
 * button", "which method?", "type your number". Nothing else is local, so a
 * status refetch can never yank the user out of a half-completed flow.
 *
 * pairMode is read under exactly two backend states: not_paired, and a terminal
 * disconnected (where relinking is the only way forward and is offered). It is
 * only ever SET from an affordance rendered in one of those two, so no other
 * branch can be reached with it non-null.
 */
export function deriveWhatsAppStep(args: {
  isLoading: boolean
  error: unknown
  status: WhatsAppStatus | undefined
  pairMode: PairMode
}): Step {
  const { isLoading, error, status, pairMode } = args

  if (error) {
    // The routes are not registered when the feature is off, so gin's own 404
    // is what "configuration required" reads.
    const statusCode =
      error instanceof Error && 'status' in error ? (error as { status: number }).status : undefined
    return statusCode === 404 ? 'not_configured' : 'fetch_error'
  }
  if (isLoading || !status) return 'loading'

  switch (status.state) {
    case 'not_ready':
      return 'not_ready'
    case 'not_paired':
      return 'not_paired'
    case 'pairing':
      return status.pairing?.method === 'phone' ? 'pairing_phone' : 'pairing_qr'
    case 'connecting':
    case 'reconnecting':
      // One branch: the user's affordances are identical (wait; there is
      // nothing to press). Only the copy differs.
      return 'connecting'
    case 'connected':
      return 'connected'
    case 'disconnected':
      // A user who took the relink affordance is in the link flow; the backend
      // state only says the previous session ended.
      return pairMode === null ? 'disconnected' : 'not_paired'
    case 'disconnect_failed':
      return 'disconnect_failed'
    case 'error':
      return 'error'
    default:
      return 'fetch_error'
  }
}

// Human sentences for the machine-readable reasons the backend persists. An
// unmapped future reason falls back to the raw value, so it still shows
// something true rather than nothing.
const REASON_SENTENCES: Record<string, string> = {
  logged_out: 'The device was unlinked from WhatsApp, so no messages can be received.',
  stream_replaced: 'Another WhatsApp session replaced this one.',
  client_outdated: 'WhatsApp rejected this client as out of date.',
  temporary_ban: 'WhatsApp temporarily restricted this account.',
  ingest_not_wired: 'The message pipeline is not ready to receive WhatsApp messages.',
  local_cleanup_failed:
    'The device is unlinked at WhatsApp, but the stored credentials could not be cleared.',
  forced_cleanup_failed: 'The stored credentials could not be cleared locally.',
  device_store_ambiguous:
    'The stored credentials could not be resolved to the linked account. Force disconnect, then link again.',
  scanned_without_multidevice:
    'The code was scanned, but multi-device mode is off on the phone. Turn it on and scan the next code.',
  passkey_pairing_unsupported:
    'The account asked to finish pairing with a passkey, which this integration cannot do.',
}

// Reasons that are terminal: the integration will not reconnect on its own, so
// linking again is the only way forward.
const TERMINAL_REASONS = new Set([
  'logged_out',
  'stream_replaced',
  'client_outdated',
  'temporary_ban',
])

function reasonSentence(reason: string | undefined): string {
  if (!reason) return 'The connection ended for an unreported reason.'
  return REASON_SENTENCES[reason] ?? reason
}

function formatTimestamp(value: string | undefined): string | null {
  if (!value) return null
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return value
  return parsed.toLocaleString()
}

export function WhatsAppSection() {
  const { data: status, error: statusError, isLoading } = useWhatsAppStatus()
  const startPairing = useStartWhatsAppPairing()
  const cancelPairing = useCancelWhatsAppPairing()
  const disconnect = useDisconnectWhatsApp()

  const [pairMode, setPairMode] = useState<PairMode>(null)
  const [phone, setPhone] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [notification, setNotification] = useState<{
    type: 'success' | 'error'
    message: string
  } | null>(null)

  const step = deriveWhatsAppStep({ isLoading, error: statusError, status, pairMode })

  useEffect(() => {
    if (notification) {
      const timer = setTimeout(() => setNotification(null), 5000)
      return () => clearTimeout(timer)
    }
  }, [notification])

  // pairMode's lifecycle is a rule, not an emergent property: it is reset —
  // together with the typed number — the moment the backend takes over the
  // flow, the moment a pairing is cancelled, and the moment an account is
  // unlinked. Without that, returning to not_paired would drop the user back
  // into the middle of a flow they just left.
  const resetPairFlow = useCallback(() => {
    setPairMode(null)
    setPhone('')
  }, [])

  const handleStartQR = useCallback(async () => {
    setError(null)
    try {
      await startPairing.mutateAsync({ method: 'qr' })
      resetPairFlow()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to start pairing')
    }
  }, [startPairing, resetPairFlow])

  const handleStartPhone = useCallback(async () => {
    setError(null)
    try {
      await startPairing.mutateAsync({ method: 'phone', phone: phone.trim() })
      resetPairFlow()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to start pairing')
    }
  }, [startPairing, phone, resetPairFlow])

  const handleCancelPairing = useCallback(async () => {
    setError(null)
    try {
      await cancelPairing.mutateAsync()
      resetPairFlow()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to cancel pairing')
    }
  }, [cancelPairing, resetPairFlow])

  const runDisconnect = useCallback(
    async (force: boolean) => {
      setError(null)
      try {
        const result = await disconnect.mutateAsync({ force })
        resetPairFlow()
        setNotification({
          type: result.warning ? 'error' : 'success',
          message: result.warning || 'WhatsApp disconnected',
        })
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to disconnect')
      }
    },
    [disconnect, resetPairFlow]
  )

  const handleDisconnect = useCallback(() => {
    if (!confirm('Disconnect WhatsApp? This unlinks the device and stops storing messages.')) return
    void runDisconnect(false)
  }, [runDisconnect])

  const handleForceClear = useCallback(() => {
    if (
      !confirm(
        'Force clear the stored WhatsApp credentials? WhatsApp is not contacted, so you must also remove this device from your phone: WhatsApp > Settings > Linked Devices.'
      )
    ) {
      return
    }
    void runDisconnect(true)
  }, [runDisconnect])

  const degraded = status ? degradedDeviceStoreNotes(status) : []

  return (
    <section aria-label="WhatsApp" className="bg-white rounded-lg shadow-sm border p-6">
      {/* Header */}
      <div className="flex items-center space-x-3 mb-2">
        <MessageCircle className="w-6 h-6 text-green-600" />
        <h2 className="text-xl font-semibold text-gray-900">WhatsApp</h2>
      </div>
      <p className="text-gray-600 text-sm mb-4">
        Link a WhatsApp account as a companion device to track message interactions.
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

      {/* 1. Loading */}
      {step === 'loading' && (
        <div
          role="status"
          aria-label="Loading WhatsApp status"
          className="flex items-center justify-center py-8"
        >
          <Loader2 className="w-6 h-6 animate-spin text-gray-400" />
        </div>
      )}

      {/* 2. Not configured */}
      {step === 'not_configured' && (
        <div className="p-4 bg-gray-50 border border-gray-200 rounded-lg">
          <div className="flex items-start gap-2">
            <Info className="w-5 h-5 text-gray-400 mt-0.5 flex-shrink-0" />
            <div>
              <p className="text-sm font-medium text-gray-700">Configuration Required</p>
              <p className="text-sm text-gray-500 mt-1">
                Set the following environment variables to enable WhatsApp integration:
              </p>
              <ul className="text-sm text-gray-500 mt-2 list-disc list-inside space-y-1">
                <li>
                  <code className="text-xs bg-gray-100 px-1 rounded">
                    ENABLE_WHATSAPP_SYNC=true
                  </code>
                </li>
                <li>
                  <code className="text-xs bg-gray-100 px-1 rounded">
                    ENABLE_EXTERNAL_SYNC=true
                  </code>
                </li>
              </ul>
              <p className="text-sm text-gray-500 mt-2">
                Both are required — the backend refuses to start with WhatsApp enabled while
                external sync is off.
              </p>
            </div>
          </div>
        </div>
      )}

      {/* 3. Fetch error */}
      {step === 'fetch_error' && (
        <div className="text-center py-6">
          <AlertCircle className="w-8 h-8 text-red-400 mx-auto mb-2" />
          <p className="text-sm text-gray-600 mb-3">Failed to load WhatsApp status</p>
          <Button variant="outline" size="sm" onClick={() => window.location.reload()}>
            Retry
          </Button>
        </div>
      )}

      {/* 4. Not ready — no link affordance at all, because the backend would
          refuse it and a disabled control invites a guess at what unlocks it. */}
      {step === 'not_ready' && status && (
        <div className="p-4 bg-amber-50 border border-amber-200 rounded-lg">
          <div className="flex items-start gap-2">
            <AlertTriangle className="w-5 h-5 text-amber-600 mt-0.5 flex-shrink-0" />
            <div>
              <p className="text-sm font-medium text-amber-800">
                WhatsApp cannot link an account yet
              </p>
              <p className="text-sm text-amber-700 mt-1">
                Waiting on: {status.missing || 'an unreported dependency'}
              </p>
            </div>
          </div>
        </div>
      )}

      {/* 5. Not paired, no method chosen */}
      {step === 'not_paired' && pairMode === null && (
        <div className="text-center py-6">
          <p className="text-sm text-gray-500 mb-4">
            Link your WhatsApp account to track message interactions automatically.
          </p>
          <Button onClick={() => setPairMode('choose')}>Link WhatsApp</Button>
        </div>
      )}

      {/* 6. Not paired, choosing a method */}
      {step === 'not_paired' && pairMode === 'choose' && (
        <div className="space-y-4">
          <p className="text-sm text-gray-600">Choose how to link this device.</p>
          <div className="flex flex-wrap gap-2">
            <Button onClick={handleStartQR} loading={startPairing.isPending}>
              <QrCode className="w-4 h-4 mr-1" />
              Scan a QR code
            </Button>
            <Button variant="secondary" onClick={() => setPairMode('phone')}>
              <Smartphone className="w-4 h-4 mr-1" />
              Use a phone pairing code
            </Button>
            <Button variant="outline" onClick={resetPairFlow}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {/* 7. Not paired, entering a phone number */}
      {step === 'not_paired' && pairMode === 'phone' && (
        <div className="space-y-4">
          <div>
            <label htmlFor="wa-phone" className="block text-sm font-medium text-gray-700 mb-1">
              Phone Number
            </label>
            <input
              id="wa-phone"
              type="tel"
              className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm text-gray-900 placeholder-gray-400 focus:outline-none focus:ring-2 focus:ring-green-500"
              placeholder="+15551234567"
              value={phone}
              onChange={e => setPhone(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && phone.trim() && handleStartPhone()}
              autoFocus
            />
            <p className="text-xs text-gray-400 mt-1">International format with country code</p>
          </div>
          <div className="flex gap-2">
            <Button
              onClick={handleStartPhone}
              loading={startPairing.isPending}
              disabled={!phone.trim()}
            >
              Send Pairing Code
            </Button>
            <Button variant="outline" onClick={resetPairFlow}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {/* 8. Pairing by QR */}
      {step === 'pairing_qr' && status?.pairing && (
        <div className="space-y-4">
          <p className="text-sm text-gray-600">
            On your phone, open WhatsApp &rarr; Settings &rarr; Linked Devices &rarr; Link a device,
            then scan this code.
          </p>
          {status.pairing.qr_code ? (
            <figure
              role="img"
              aria-label="WhatsApp pairing QR code"
              // The accessible name is the human description, never the code
              // string: a screen reader announcing an opaque blob is worse than
              // useless. The code is exposed on a value attribute instead, so a
              // test can prove it reached the DOM without decoding a matrix.
              data-qr-value={status.pairing.qr_code}
              className="inline-block bg-white p-3 border border-gray-200 rounded-lg"
            >
              <QRCodeSVG value={status.pairing.qr_code} size={200} />
            </figure>
          ) : (
            <p className="text-sm text-gray-500">Waiting for a code from WhatsApp…</p>
          )}
          <p className="text-xs text-gray-400">
            Codes refresh automatically. This one expires{' '}
            {formatTimestamp(status.pairing.expires_at)}.
          </p>
          <Button variant="outline" onClick={handleCancelPairing} loading={cancelPairing.isPending}>
            Cancel
          </Button>
        </div>
      )}

      {/* 9. Pairing by phone code */}
      {step === 'pairing_phone' && status?.pairing && (
        <div className="space-y-4">
          <p className="text-sm text-gray-600">
            On your phone, open WhatsApp &rarr; Settings &rarr; Linked Devices &rarr; Link with
            phone number instead, then type this code.
          </p>
          {status.pairing.pair_code ? (
            <p className="text-2xl font-mono tracking-widest text-gray-900">
              {status.pairing.pair_code}
            </p>
          ) : (
            <p className="text-sm text-gray-500">Waiting for a code from WhatsApp…</p>
          )}
          <p className="text-xs text-gray-400">
            This code expires {formatTimestamp(status.pairing.expires_at)}.
          </p>
          <Button variant="outline" onClick={handleCancelPairing} loading={cancelPairing.isPending}>
            Cancel
          </Button>
        </div>
      )}

      {/* 10. Connecting / reconnecting */}
      {step === 'connecting' && status && (
        <div className="flex items-center gap-2 py-6">
          <Loader2 className="w-5 h-5 animate-spin text-gray-400" />
          <span className="text-sm text-gray-600">
            {status.state === 'reconnecting'
              ? 'Reconnecting to WhatsApp…'
              : 'Connecting to WhatsApp…'}
          </span>
        </div>
      )}

      {/* 11. Connected */}
      {step === 'connected' && status && (
        <div className="space-y-4">
          <div className="flex items-center gap-2">
            <CheckCircle className="w-5 h-5 text-green-500" />
            <span className="text-sm font-medium text-gray-900">
              Connected{status.push_name ? ` as ${status.push_name}` : ''}
            </span>
          </div>
          {status.phone_number && (
            <p className="text-sm text-gray-500">Phone: {status.phone_number}</p>
          )}
          {status.jid && <p className="text-sm text-gray-500">Device: {status.jid}</p>}
          {status.connected_at && (
            <p className="text-sm text-gray-500">
              Connected since {formatTimestamp(status.connected_at)}
            </p>
          )}

          {degraded.length > 0 && <DegradedDeviceStoreAdvisory notes={degraded} />}

          {/* Backfill progress */}
          <div className="p-3 bg-blue-50 border border-blue-100 rounded-lg space-y-1">
            {status.backfill.pending + status.backfill.processing > 0 ? (
              <div className="flex items-center gap-2">
                <Loader2 className="w-4 h-4 animate-spin text-blue-500" />
                <span className="text-sm font-medium text-blue-700">
                  Importing message history… {status.backfill.pending + status.backfill.processing}{' '}
                  chunks remaining
                  {status.backfill.stale ? ' (counts may be out of date)' : ''}
                </span>
              </div>
            ) : (
              <p className="text-sm text-blue-700">
                Message history import is idle.
                {status.backfill.stale ? ' Counts may be out of date.' : ''}
              </p>
            )}
            {status.backfill.failed > 0 && (
              <p className="text-sm text-blue-600">
                {status.backfill.failed} chunk(s) could not be imported. They are kept and can be
                retried by hand.
              </p>
            )}
            {status.backfill.observed_floor_at && (
              <p className="text-sm text-blue-600">
                History reaches back to {formatTimestamp(status.backfill.observed_floor_at)}.
              </p>
            )}
          </div>

          {/* Dropped one-shot history — an accepted gap, stated rather than hidden */}
          {status.backfill.dropped_inline_chunks > 0 && (
            <div className="p-3 bg-amber-50 border border-amber-200 rounded-lg">
              <div className="flex items-start gap-2">
                <AlertTriangle className="w-4 h-4 text-amber-600 mt-0.5 flex-shrink-0" />
                <div className="text-sm text-amber-800">
                  <p className="font-medium">
                    {status.backfill.dropped_inline_chunks} chunk(s) of message history were dropped
                  </p>
                  <p className="mt-1 text-amber-700">
                    WhatsApp delivered them in a form this integration cannot import. Those messages
                    are not recoverable without unlinking and pairing again. This is a known,
                    accepted limitation.
                  </p>
                </div>
              </div>
            </div>
          )}

          <p className="text-sm text-gray-500">
            Unidentified peers observed: {status.ingest.unresolved_lid_peers}
          </p>

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

          <WhatsAppChatList />
        </div>
      )}

      {/* 12. Disconnected */}
      {step === 'disconnected' && status && (
        <div className="space-y-3">
          <div className="p-4 bg-gray-50 border border-gray-200 rounded-lg">
            <p className="text-sm font-medium text-gray-700">WhatsApp is disconnected</p>
            <p className="text-sm text-gray-600 mt-1">{reasonSentence(status.reason)}</p>
            {status.reason === 'temporary_ban' && status.banned_until && (
              <p className="text-sm text-gray-600 mt-1">
                The restriction lifts {formatTimestamp(status.banned_until)}.
              </p>
            )}
          </div>
          {status.reason && TERMINAL_REASONS.has(status.reason) && (
            <Button onClick={() => setPairMode('choose')}>Link WhatsApp again</Button>
          )}
        </div>
      )}

      {/* 13. Disconnect failed */}
      {step === 'disconnect_failed' && status && (
        <div className="space-y-3">
          <div className="p-4 bg-red-50 border border-red-200 rounded-lg">
            <div className="flex items-start gap-2">
              <AlertCircle className="w-5 h-5 text-red-500 mt-0.5 flex-shrink-0" />
              <div>
                <p className="text-sm font-medium text-red-800">The unlink did not complete</p>
                <p className="text-sm text-red-700 mt-1">
                  WhatsApp did not confirm the unlink, so the stored credentials were deliberately
                  kept and you can retry. {reasonSentence(status.reason)}
                </p>
              </div>
            </div>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" onClick={handleDisconnect} loading={disconnect.isPending}>
              Retry disconnect
            </Button>
            <Button variant="danger" onClick={handleForceClear} loading={disconnect.isPending}>
              Force clear
            </Button>
          </div>
          <p className="text-xs text-gray-500">
            A forced clear contacts WhatsApp not at all, so you must also remove this device from
            your phone: WhatsApp &rarr; Settings &rarr; Linked Devices.
          </p>
        </div>
      )}

      {/* 14. Error */}
      {step === 'error' && status && (
        <div className="p-4 bg-red-50 border border-red-200 rounded-lg">
          <div className="flex items-start gap-2">
            <AlertCircle className="w-5 h-5 text-red-500 mt-0.5 flex-shrink-0" />
            <div>
              <p className="text-sm font-medium text-red-800">WhatsApp could not start</p>
              <p className="text-sm text-red-700 mt-1">{reasonSentence(status.reason)}</p>
            </div>
          </div>
        </div>
      )}

      {/* Configuration info box */}
      <div className="mt-6 p-4 bg-blue-50 border border-blue-100 rounded-lg">
        <div className="flex items-start gap-2">
          <Info className="w-4 h-4 text-blue-500 mt-0.5 flex-shrink-0" />
          <div className="text-sm text-blue-700">
            <p className="font-medium">About WhatsApp Integration</p>
            <p className="mt-1 text-blue-600">
              This links a companion device and only reads. It never sends a message, never marks a
              chat as read, and never signals that you are online. Messages are processed locally
              and never sent to external services.
            </p>
          </div>
        </div>
      </div>
    </section>
  )
}

// degradedDeviceStoreNotes collects the symptoms of one condition — the device
// store is in a state the user should know about — which share one remedy.
function degradedDeviceStoreNotes(status: WhatsAppStatus): string[] {
  const notes: string[] = []
  if (status.replaced_device_retained) {
    notes.push(
      'A previous device could not be removed when this one was linked, so a stale session is still stored.'
    )
  }
  if (status.terminal_reason_persisted === false) {
    notes.push(
      'The reason this connection ended could not be recorded, so it will not survive a restart.'
    )
  }
  if (status.link_selector_persisted === false) {
    notes.push(
      'The record of which device is linked could not be written, so a restart may not resolve it.'
    )
  }
  return notes
}

function DegradedDeviceStoreAdvisory({ notes }: { notes: string[] }) {
  return (
    <div className="p-3 bg-amber-50 border border-amber-200 rounded-lg">
      <div className="flex items-start gap-2">
        <AlertTriangle className="w-4 h-4 text-amber-600 mt-0.5 flex-shrink-0" />
        <div className="text-sm text-amber-800">
          <p className="font-medium">The stored WhatsApp device state is degraded</p>
          <ul className="mt-1 list-disc list-inside space-y-1 text-amber-700">
            {notes.map(note => (
              <li key={note}>{note}</li>
            ))}
          </ul>
          <p className="mt-1 text-amber-700">
            Force disconnect clears it; link the account again afterwards.
          </p>
        </div>
      </div>
    </div>
  )
}

function WhatsAppChatList() {
  const { data: chats, isLoading, error } = useWhatsAppChats()
  const updateStatus = useUpdateWhatsAppChatStatus()

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
          No group chats discovered yet. Groups appear here once WhatsApp messages arrive from them.
        </p>
      </div>
    )
  }

  return (
    <div className="mt-4 space-y-1">
      <h3 className="text-sm font-medium text-gray-700 mb-1">Group Chats</h3>
      <p className="text-xs text-gray-500 mb-2">
        Turning a group on starts storing new messages only — messages received while it was
        untracked were never stored and cannot be recovered.
      </p>
      {updateStatus.isError && (
        <div className="flex items-center gap-2 p-2 rounded bg-red-50 text-red-600 text-sm mb-2">
          <AlertCircle className="w-4 h-4 flex-shrink-0" />
          Failed to update chat status
        </div>
      )}
      {chats.map(chat => {
        // A group can only be missing its title if the server returned an empty
        // one. Falling back to the JID keeps two unnamed groups distinguishable,
        // which "Untitled" would not.
        const label = chat.chat_title || chat.chat_jid
        return (
          <div
            key={chat.chat_jid}
            className="flex items-center justify-between p-3 bg-gray-50 rounded-lg"
          >
            <div className="flex items-center gap-2 min-w-0">
              <span
                className={`w-2 h-2 rounded-full flex-shrink-0 ${
                  chat.effective_tracked ? 'bg-green-500' : 'bg-gray-300'
                }`}
              />
              <span className="text-sm text-gray-900 truncate">{label}</span>
              <span className="text-xs text-gray-400 flex-shrink-0">
                {chat.member_count != null ? `${chat.member_count} members` : 'size unknown'}
              </span>
            </div>
            <select
              value={chat.status}
              aria-label={`Tracking for ${label}`}
              onChange={e =>
                updateStatus.mutate({
                  chatJid: chat.chat_jid,
                  status: e.target.value as WhatsAppChatStatus,
                })
              }
              className="text-sm text-gray-700 bg-white border border-gray-300 rounded px-2 py-1 focus:outline-none focus:ring-2 focus:ring-green-500"
            >
              <option value="auto">Auto</option>
              <option value="tracked">Tracked</option>
              <option value="ignored">Ignored</option>
            </select>
          </div>
        )
      })}
    </div>
  )
}
