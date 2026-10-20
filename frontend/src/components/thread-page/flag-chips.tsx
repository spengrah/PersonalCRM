'use client'

import { useState } from 'react'
import { clsx } from 'clsx'
import { Check, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { saveItem, useFailedSave, usePendingSaves, useLabelFlag } from '@/hooks/use-bench'
import type { FlagChipResponse, NearMissResponse } from '@/types/generated/contact'
import type { ChipState, FlagKind } from '@/types/generated/judgment'
import { FLAG_KINDS, FLAG_KIND_ORDER } from '@/components/threads/flag-kinds'
import { SAVE_ERROR, formatConfidence } from '@/components/threads/format'
import { formatDay } from './night-label'

const STATE_LOOK: Record<ChipState, { box: string; label: string; text: string }> = {
  to_review: {
    box: 'border border-dashed border-amber-700 bg-amber-50',
    label: 'To review',
    text: 'text-amber-800',
  },
  confirmed: {
    box: 'border border-green-300 bg-green-50',
    label: 'Confirmed',
    text: 'text-green-800',
  },
  removed: { box: 'border border-gray-200 bg-gray-50', label: 'Removed', text: 'text-gray-600' },
  added: { box: 'border border-green-300 bg-green-50', label: 'Added', text: 'text-green-800' },
}

interface MessageTarget {
  threadId: string
  runId: string
  messageKey: string
}

function Chip({ target, chip }: { target: MessageTarget; chip: FlagChipResponse }) {
  const [changing, setChanging] = useState(false)
  const label = useLabelFlag()
  const isPending = usePendingSaves()
  const pending = isPending(saveItem.flag(target.messageKey, chip.kind))
  const { name, icon: Icon, iconClass } = FLAG_KINDS[chip.kind]
  const look = STATE_LOOK[chip.state]
  const reviewing = chip.state === 'to_review' || changing

  const send = (holds: boolean) =>
    label.mutate({ ...target, kind: chip.kind, holds }, { onSuccess: () => setChanging(false) })

  return (
    <span
      data-chip={chip.kind}
      data-chip-state={chip.state}
      className={clsx('inline-flex items-center gap-1.5 rounded-md px-2 py-1 text-xs', look.box)}
    >
      <Icon aria-hidden="true" className={clsx('h-3.5 w-3.5', iconClass)} />
      <span
        data-chip-name
        className={clsx(
          'font-medium',
          chip.state === 'removed' ? 'text-gray-500 line-through' : 'text-gray-900'
        )}
      >
        {name}
      </span>
      {chip.probability != null && (
        <span data-chip-probability className="text-gray-600">
          {formatConfidence(chip.probability)}
        </span>
      )}
      <span data-chip-label className={clsx('font-medium', look.text)}>
        {look.label}
      </span>
      {chip.cleared_at && (
        <span data-chip-cleared className="font-medium text-green-800">
          cleared {formatDay(chip.cleared_at)}
        </span>
      )}
      {reviewing && (
        <>
          <button
            type="button"
            aria-label={`Confirm ${name}`}
            disabled={pending}
            onClick={() => send(true)}
            className="flex h-8 w-8 items-center justify-center rounded-md border border-gray-300 bg-white text-green-800 hover:bg-gray-50 disabled:opacity-50"
          >
            <Check aria-hidden="true" className="h-4 w-4" />
          </button>
          <button
            type="button"
            aria-label={`Remove ${name}`}
            disabled={pending}
            onClick={() => send(false)}
            className="flex h-8 w-8 items-center justify-center rounded-md border border-gray-300 bg-white text-gray-700 hover:bg-gray-50 disabled:opacity-50"
          >
            <X aria-hidden="true" className="h-4 w-4" />
          </button>
        </>
      )}
      {chip.state !== 'to_review' && (
        <button
          type="button"
          aria-label={changing ? undefined : `Change ${name}`}
          disabled={pending}
          onClick={() => setChanging(!changing)}
          className="rounded-md px-1 text-sm font-medium text-blue-600 hover:bg-blue-50 hover:text-blue-800 disabled:opacity-50"
        >
          {changing ? 'Cancel' : 'Change'}
        </button>
      )}
    </span>
  )
}

function FlagMenu({ target, kinds }: { target: MessageTarget; kinds: FlagKind[] }) {
  const [open, setOpen] = useState(false)
  const label = useLabelFlag()
  if (kinds.length === 0) return null

  return (
    <span className="relative inline-block">
      <Button
        type="button"
        size="sm"
        variant="outline"
        aria-haspopup="menu"
        aria-expanded={open}
        className="!px-2 !py-1 text-xs"
        onClick={() => setOpen(!open)}
      >
        + Flag
      </Button>
      {open && (
        <div
          role="menu"
          aria-label="Add a flag"
          className="absolute left-0 top-full z-10 mt-1 min-w-[10rem] rounded-md border border-gray-200 bg-white py-1 shadow-lg"
        >
          {kinds.map(kind => {
            const { name, icon: Icon, iconClass } = FLAG_KINDS[kind]
            return (
              <button
                key={kind}
                type="button"
                role="menuitem"
                onClick={() => {
                  setOpen(false)
                  label.mutate({ ...target, kind, holds: true })
                }}
                className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm text-gray-700 hover:bg-gray-50"
              >
                <Icon aria-hidden="true" className={clsx('h-4 w-4', iconClass)} />
                {name}
              </button>
            )
          })}
        </div>
      )}
    </span>
  )
}

/**
 * The kind a message showing no chip came closest to flagging, muted and
 * display only. "+ Flag" adds it when the message does hold it.
 */
function NearMiss({ nearMiss }: { nearMiss: NearMissResponse }) {
  const { name, icon: Icon } = FLAG_KINDS[nearMiss.kind]
  return (
    <span
      data-near-miss={nearMiss.kind}
      title="Closest flag, below the display threshold"
      className="inline-flex items-center gap-1 text-xs text-gray-500"
    >
      <Icon aria-hidden="true" className="h-3.5 w-3.5" />
      {name} {formatConfidence(nearMiss.probability)}
    </span>
  )
}

/**
 * A new message's flag chips and its "+ Flag" control. The chips and their
 * states are the server's: a control saves a label through the one ordered
 * queue and the chip then shows what the server recorded. "+ Flag" offers each
 * kind the message shows no chip for. A message showing no chip may carry the
 * server's near miss.
 */
export function MessageFlags({
  threadId,
  runId,
  messageKey,
  chips,
  nearMiss,
}: MessageTarget & { chips: FlagChipResponse[]; nearMiss?: NearMissResponse }) {
  const isFailed = useFailedSave()
  const failed = FLAG_KIND_ORDER.some(kind => isFailed(saveItem.flag(messageKey, kind)))
  const target = { threadId, runId, messageKey }
  const shown = new Set(chips.map(chip => chip.kind))
  return (
    <div data-message-flags className="flex flex-wrap items-center gap-2 pt-1">
      {chips.map(chip => (
        <Chip key={chip.kind} target={target} chip={chip} />
      ))}
      {nearMiss && <NearMiss nearMiss={nearMiss} />}
      <FlagMenu target={target} kinds={FLAG_KIND_ORDER.filter(kind => !shown.has(kind))} />
      {failed && <span className="text-xs text-red-700">{SAVE_ERROR}</span>}
    </div>
  )
}
