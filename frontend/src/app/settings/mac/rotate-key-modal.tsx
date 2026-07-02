'use client'

import { useState } from 'react'
import { Copy } from 'lucide-react'

import { Button } from '@/components/ui/button'

export interface RotateKeyModalProps {
  hostname: string
  token: { token: string; expires_at: string } | null
  isPending: boolean
  isError: boolean
  onClose: () => void
}

/**
 * RotateKeyModal mints + displays a single-use pairing token plus
 * the templated `crm-mac install --re-pair --pair <token>` command
 * the operator runs on the Mac. The browser never holds the
 * daemon's current pair-key — the rotation itself runs on the Mac,
 * authenticated by the daemon's current key + the token shown here.
 */
export function RotateKeyModal({
  hostname,
  token,
  isPending,
  isError,
  onClose,
}: RotateKeyModalProps) {
  const [copied, setCopied] = useState(false)

  const fullCommand = token ? `crm-mac install --re-pair --pair ${token.token}` : ''

  const handleCopy = async () => {
    if (!token) return
    try {
      await navigator.clipboard.writeText(fullCommand)
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
      aria-label={`Rotate pair-key for ${hostname}`}
    >
      <div className="bg-white rounded-lg shadow-lg max-w-md w-full m-4 p-6">
        <h2 className="text-xl font-semibold text-gray-900 mb-4">Rotate pair-key for {hostname}</h2>
        {isPending ? (
          <p className="text-gray-600">Generating pairing token...</p>
        ) : isError ? (
          <p className="text-red-700">Failed to mint pairing token. Please try again.</p>
        ) : token ? (
          <div className="space-y-3">
            <p className="text-sm text-gray-700">
              Run this on the Mac to rotate its api-key. The daemon will restart automatically; no
              re-install, no re-grant of permissions required. The current api-key stops working the
              moment the rotation completes.
            </p>
            <div className="flex items-center space-x-2">
              <code
                className="flex-1 min-w-0 px-3 py-2 bg-gray-100 rounded text-sm font-mono text-gray-900 break-all"
                data-testid="rotate-key-command"
              >
                {fullCommand}
              </code>
              <Button variant="outline" size="sm" onClick={handleCopy}>
                <Copy className="w-4 h-4 mr-1" /> {copied ? 'Copied' : 'Copy'}
              </Button>
            </div>
            <p className="text-xs text-gray-600">
              Token expires at {new Date(token.expires_at).toLocaleString()} and can be used only
              once.
            </p>
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
