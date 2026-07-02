'use client'

import { useEffect, useState } from 'react'
import Link from 'next/link'
import { Cpu, ArrowLeft, RefreshCw, Trash2, Copy, AlertTriangle, KeyRound } from 'lucide-react'

import { Navigation } from '@/components/layout/navigation'
import { Button } from '@/components/ui/button'
import {
  useCreatePairingToken,
  useDeleteMacHost,
  useMacHostSourceCounts,
  useMacHosts,
} from '@/hooks/use-mac-hosts'
import type { MacHost } from '@/lib/mac-hosts-api'
import { renderCursorCell, type SourceHealthEntry } from './cursor-cell'
import { RotateKeyModal } from './rotate-key-modal'

const HEARTBEAT_FRESH_MS = 5 * 60 * 1000
const HEARTBEAT_STALE_MS = 30 * 60 * 1000

type HeartbeatHealth = 'fresh' | 'stale' | 'lost' | 'never'

function heartbeatHealth(host: MacHost, now: number): HeartbeatHealth {
  if (!host.last_heartbeat_at) return 'never'
  const last = Date.parse(host.last_heartbeat_at)
  if (Number.isNaN(last)) return 'never'
  const age = now - last
  if (age <= HEARTBEAT_FRESH_MS) return 'fresh'
  if (age <= HEARTBEAT_STALE_MS) return 'stale'
  return 'lost'
}

function heartbeatLabel(host: MacHost, now: number): string {
  if (!host.last_heartbeat_at) return 'Never'
  const last = Date.parse(host.last_heartbeat_at)
  if (Number.isNaN(last)) return 'Never'
  const ageSec = Math.max(0, Math.floor((now - last) / 1000))
  if (ageSec < 60) return `${ageSec}s ago`
  if (ageSec < 3600) return `${Math.floor(ageSec / 60)}m ago`
  return `${Math.floor(ageSec / 3600)}h ago`
}

const healthBadgeClass: Record<HeartbeatHealth, string> = {
  fresh: 'bg-green-100 text-green-800 border-green-300',
  stale: 'bg-yellow-100 text-yellow-800 border-yellow-300',
  lost: 'bg-red-100 text-red-800 border-red-300',
  never: 'bg-gray-100 text-gray-700 border-gray-300',
}

const healthBadgeLabel: Record<HeartbeatHealth, string> = {
  fresh: 'Healthy',
  stale: 'Stale',
  lost: 'Lost',
  never: 'No heartbeat',
}

export default function MacSettingsPage() {
  const { data: hosts, isLoading, refetch } = useMacHosts()
  const deleteHost = useDeleteMacHost()
  const createToken = useCreatePairingToken()

  const [now, setNow] = useState(() => Date.now())
  const [pairingModalOpen, setPairingModalOpen] = useState(false)
  const [pairingToken, setPairingToken] = useState<{ token: string; expires_at: string } | null>(
    null
  )
  const [confirmDeleteId, setConfirmDeleteId] = useState<string | null>(null)
  // rotateKeyForHost is the host whose Rotate Key button was clicked;
  // null when the modal is closed. Track the full host (not just id) so
  // the modal can display the hostname without re-fetching.
  const [rotateKeyForHost, setRotateKeyForHost] = useState<MacHost | null>(null)
  const [rotateKeyToken, setRotateKeyToken] = useState<{
    token: string
    expires_at: string
  } | null>(null)

  // Tick `now` once a second so the relative-time labels stay current.
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(id)
  }, [])

  const handlePair = async () => {
    setPairingModalOpen(true)
    try {
      const tok = await createToken.mutateAsync()
      setPairingToken(tok)
    } catch (err) {
      console.error('pairing token mint failed', err)
    }
  }

  const handleClosePairingModal = () => {
    setPairingModalOpen(false)
    setPairingToken(null)
  }

  const handleRotateKey = async (host: MacHost) => {
    setRotateKeyForHost(host)
    setRotateKeyToken(null)
    try {
      const tok = await createToken.mutateAsync()
      setRotateKeyToken(tok)
    } catch (err) {
      console.error('rotate-key token mint failed', err)
    }
  }

  const handleCloseRotateKeyModal = () => {
    setRotateKeyForHost(null)
    setRotateKeyToken(null)
  }

  const handleConfirmDelete = async () => {
    if (!confirmDeleteId) return
    try {
      await deleteHost.mutateAsync(confirmDeleteId)
      setConfirmDeleteId(null)
    } catch (err) {
      console.error('revoke host failed', err)
    }
  }

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
        <div className="mb-6">
          <Link
            href="/settings"
            className="inline-flex items-center text-sm text-gray-600 hover:text-gray-900"
          >
            <ArrowLeft className="w-4 h-4 mr-1" />
            Back to Settings
          </Link>
        </div>

        <div className="mb-8 flex items-start justify-between">
          <div>
            <div className="flex items-center space-x-3 mb-2">
              <Cpu className="w-8 h-8 text-blue-600" />
              <h1 className="text-3xl font-bold text-gray-900">Mac Daemon</h1>
            </div>
            <p className="text-lg text-gray-600">
              Paired Mac hosts that push data to your CRM (e.g. Messages, iCloud Contacts).
            </p>
          </div>
          <div className="flex items-center space-x-2">
            <Button variant="outline" size="sm" onClick={() => refetch()}>
              <RefreshCw className="w-4 h-4 mr-1" /> Refresh
            </Button>
            <Button onClick={handlePair}>Pair new Mac</Button>
          </div>
        </div>

        <section className="bg-white rounded-lg shadow-sm border">
          <div className="p-6">
            {isLoading ? (
              <p className="text-gray-500">Loading paired hosts...</p>
            ) : !hosts || hosts.length === 0 ? (
              <div className="text-center py-12">
                <Cpu className="w-12 h-12 text-gray-300 mx-auto mb-3" />
                <p className="text-lg font-medium text-gray-900">No Mac hosts paired</p>
                <p className="text-sm text-gray-600 mt-1">
                  Pair a Mac to start syncing Messages and iCloud Contacts.
                </p>
              </div>
            ) : (
              <ul className="divide-y divide-gray-200">
                {hosts.map(host => {
                  const health = heartbeatHealth(host, now)
                  return (
                    <li
                      key={host.id}
                      className="py-4 first:pt-0 last:pb-0"
                      data-testid="mac-host-row"
                    >
                      <div className="flex items-start justify-between">
                        <div className="min-w-0 flex-1">
                          <div className="flex items-center space-x-3 mb-1">
                            <h3 className="text-lg font-semibold text-gray-900 truncate">
                              {host.hostname}
                            </h3>
                            <span
                              className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${healthBadgeClass[health]}`}
                            >
                              {healthBadgeLabel[health]}
                            </span>
                          </div>
                          <dl className="text-sm text-gray-600 space-y-1">
                            <div>
                              <dt className="inline font-medium">Host ID:</dt>{' '}
                              <dd className="inline font-mono text-xs">{host.id}</dd>
                            </div>
                            <div>
                              <dt className="inline font-medium">Daemon version:</dt>{' '}
                              <dd className="inline">{host.daemon_version || 'unknown'}</dd>
                              <span className="mx-2 text-gray-300">·</span>
                              <dt className="inline font-medium">Protocol:</dt>{' '}
                              <dd className="inline">v{host.protocol_version}</dd>
                              <span className="mx-2 text-gray-300">·</span>
                              <dt className="inline font-medium">Cursor epoch:</dt>{' '}
                              <dd className="inline">{host.cursor_epoch}</dd>
                            </div>
                            <div>
                              <dt className="inline font-medium">Last heartbeat:</dt>{' '}
                              <dd className="inline" title={host.last_heartbeat_at ?? 'never'}>
                                {heartbeatLabel(host, now)}
                              </dd>
                            </div>
                            {host.api_key_rotated_at ? (
                              <div>
                                <dt className="inline font-medium">Last rotated:</dt>{' '}
                                <dd className="inline">
                                  {new Date(host.api_key_rotated_at).toLocaleString()}
                                </dd>
                              </div>
                            ) : null}
                          </dl>
                          <PermissionsBadges permissions={host.permissions} />
                          <SourceHealthTable health={host.source_health} hostId={host.id} />
                        </div>
                        <div className="flex items-center space-x-2 flex-shrink-0">
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() => handleRotateKey(host)}
                            aria-label={`Rotate pair-key for ${host.hostname}`}
                          >
                            <KeyRound className="w-4 h-4 mr-1" /> Rotate Key
                          </Button>
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() => setConfirmDeleteId(host.id)}
                            aria-label={`Uninstall ${host.hostname}`}
                          >
                            <Trash2 className="w-4 h-4 mr-1" /> Uninstall
                          </Button>
                        </div>
                      </div>
                    </li>
                  )
                })}
              </ul>
            )}
          </div>
        </section>
      </div>

      {pairingModalOpen ? (
        <PairingTokenModal
          token={pairingToken}
          isPending={createToken.isPending}
          isError={createToken.isError}
          onClose={handleClosePairingModal}
        />
      ) : null}

      {rotateKeyForHost ? (
        <RotateKeyModal
          hostname={rotateKeyForHost.hostname}
          token={rotateKeyToken}
          isPending={createToken.isPending}
          isError={createToken.isError}
          onClose={handleCloseRotateKeyModal}
        />
      ) : null}

      {confirmDeleteId ? (
        <ConfirmDeleteModal
          isPending={deleteHost.isPending}
          onCancel={() => setConfirmDeleteId(null)}
          onConfirm={handleConfirmDelete}
        />
      ) : null}
    </div>
  )
}

interface PairingTokenModalProps {
  token: { token: string; expires_at: string } | null
  isPending: boolean
  isError: boolean
  onClose: () => void
}

function PairingTokenModal({ token, isPending, isError, onClose }: PairingTokenModalProps) {
  const [copied, setCopied] = useState(false)

  const handleCopy = async () => {
    if (!token) return
    try {
      await navigator.clipboard.writeText(token.token)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch (err) {
      console.error('clipboard write failed', err)
    }
  }

  return (
    <div
      className="fixed inset-0 bg-black/50 flex items-center justify-center z-50"
      role="dialog"
      aria-modal="true"
      aria-label="Pair new Mac"
    >
      <div className="bg-white rounded-lg shadow-lg max-w-md w-full m-4 p-6">
        <h2 className="text-xl font-semibold text-gray-900 mb-4">Pair new Mac</h2>
        {isPending ? (
          <p className="text-gray-600">Generating pairing token...</p>
        ) : isError ? (
          <p className="text-red-700">Failed to mint pairing token. Please try again.</p>
        ) : token ? (
          <div className="space-y-3">
            <p className="text-sm text-gray-700">
              Copy this token and paste it into the Mac daemon&apos;s setup prompt. It expires at{' '}
              {new Date(token.expires_at).toLocaleString()} and can be used only once.
            </p>
            <div className="flex items-center space-x-2">
              <code
                className="flex-1 min-w-0 px-3 py-2 bg-gray-100 rounded text-sm font-mono text-gray-900 break-all"
                data-testid="pairing-token-value"
              >
                {token.token}
              </code>
              <Button variant="outline" size="sm" onClick={handleCopy}>
                <Copy className="w-4 h-4 mr-1" /> {copied ? 'Copied' : 'Copy'}
              </Button>
            </div>
          </div>
        ) : null}
        <div className="mt-6 flex justify-end">
          <Button variant="outline" onClick={onClose}>
            Close
          </Button>
        </div>
      </div>
    </div>
  )
}

interface ConfirmDeleteModalProps {
  isPending: boolean
  onCancel: () => void
  onConfirm: () => void
}

function ConfirmDeleteModal({ isPending, onCancel, onConfirm }: ConfirmDeleteModalProps) {
  return (
    <div
      className="fixed inset-0 bg-black/50 flex items-center justify-center z-50"
      role="dialog"
      aria-modal="true"
      aria-label="Uninstall Mac host"
    >
      <div className="bg-white rounded-lg shadow-lg max-w-md w-full m-4 p-6">
        <div className="flex items-start space-x-3 mb-4">
          <AlertTriangle className="w-6 h-6 text-yellow-500 flex-shrink-0" />
          <div>
            <h2 className="text-xl font-semibold text-gray-900 mb-1">Uninstall Mac host?</h2>
            <p className="text-sm text-gray-700">
              The daemon will be locked out on its next request. You&apos;ll need a fresh pairing
              token to re-pair. Sync cursor state for this host will also be removed.
            </p>
          </div>
        </div>
        <div className="flex justify-end space-x-2">
          <Button variant="outline" onClick={onCancel} disabled={isPending}>
            Cancel
          </Button>
          <Button onClick={onConfirm} disabled={isPending}>
            {isPending ? 'Uninstalling...' : 'Uninstall'}
          </Button>
        </div>
      </div>
    </div>
  )
}

// Known permission keys the daemon advertises in heartbeat. Unknown
// keys are listed as text so the operator can see what the daemon
// reported even if the schema evolves before the UI catches up.
const KNOWN_PERMISSIONS: Record<string, string> = {
  fda: 'Full Disk Access',
  contacts: 'Contacts',
  files_anarlog: 'Files (Anarlog)',
}

interface PermissionsBadgesProps {
  permissions: Record<string, unknown>
}

function PermissionsBadges({ permissions }: PermissionsBadgesProps) {
  const entries = Object.entries(permissions || {})
  if (entries.length === 0) return null
  return (
    <div className="mt-3" data-testid="permissions-badges">
      <p className="text-xs font-medium text-gray-700 mb-1">Permissions</p>
      <div className="flex flex-wrap gap-2">
        {entries.map(([key, value]) => {
          const granted = Boolean(value)
          const label = KNOWN_PERMISSIONS[key] ?? key
          return (
            <span
              key={key}
              className={`inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium border ${
                granted
                  ? 'bg-green-50 text-green-800 border-green-200'
                  : 'bg-gray-100 text-gray-600 border-gray-200'
              }`}
            >
              {label}: {granted ? 'granted' : 'denied'}
            </span>
          )
        })}
      </div>
    </div>
  )
}

interface SourceHealthTableProps {
  health: Record<string, unknown>
  hostId: string
}

// Source-name → human-readable label. Sources not in this map render
// the raw source string (matches the pre-fix behaviour for unknown
// sources).
const SOURCE_LABELS: Record<string, string> = {
  messages: 'Messages',
  icloud_contacts: 'iCloud Contacts',
  phone_calls: 'Phone & FaceTime',
}

function SourceHealthTable({ health, hostId }: SourceHealthTableProps) {
  const entries = Object.entries(health || {})
  const { data: counts } = useMacHostSourceCounts(hostId)
  if (entries.length === 0) return null
  return (
    <div className="mt-3" data-testid="source-health">
      <p className="text-xs font-medium text-gray-700 mb-1">Source health</p>
      <table className="w-full text-xs border border-gray-200 rounded">
        <thead className="bg-gray-50 text-gray-700">
          <tr>
            <th className="text-left px-2 py-1 font-medium">Source</th>
            <th className="text-left px-2 py-1 font-medium">Last pushed</th>
            <th className="text-left px-2 py-1 font-medium">Cursor</th>
            <th className="text-left px-2 py-1 font-medium">Error</th>
          </tr>
        </thead>
        <tbody>
          {entries.map(([source, raw]) => {
            const entry =
              typeof raw === 'object' && raw !== null
                ? (raw as SourceHealthEntry)
                : ({} as SourceHealthEntry)
            const label = SOURCE_LABELS[source] ?? source
            return (
              <tr key={source} className="border-t border-gray-100">
                <td className="px-2 py-1">{label}</td>
                <td className="px-2 py-1 text-gray-700">
                  {entry.last_pushed_at ? new Date(entry.last_pushed_at).toLocaleString() : '—'}
                </td>
                <td className="px-2 py-1 font-mono text-gray-700 truncate max-w-xs">
                  {renderCursorCell(source, entry, counts)}
                </td>
                <td className="px-2 py-1 text-red-700">
                  {entry.last_error && entry.last_error !== '' ? entry.last_error : ''}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
