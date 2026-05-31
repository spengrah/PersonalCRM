'use client'

import { useState } from 'react'
import { MessageCircle, UserPlus, Ban } from 'lucide-react'
import { Button } from '@/components/ui/button'
import type { NameCandidateGroup } from '@/types/import'

interface NameCandidateRowProps {
  group: NameCandidateGroup
  busy?: boolean
  /** Opens the name-candidate modal (which hosts the Import/Link toggle). */
  onCreate: (group: NameCandidateGroup) => void
  /** Ignores the whole token group ("Not a person"). */
  onIgnore: (group: NameCandidateGroup) => void
}

/**
 * A name-candidate row: a name lifted from session titles, ranked by evidence.
 * Dashed avatar + "from title · low confidence" chip make clear it is NOT
 * a confirmed contact. "Create contact…" opens the extended modal (which
 * hosts the Import/Link toggle); "Not a person" ignores the token group.
 */
export function NameCandidateRow({ group, busy, onCreate, onIgnore }: NameCandidateRowProps) {
  const [open, setOpen] = useState(false)
  const titles = group.session_titles ?? []
  const count = group.evidence_count

  return (
    <div
      data-testid={`name-candidate-row-${group.normalized_token}`}
      className="rounded-lg border border-gray-200 bg-white px-3.5 py-3"
    >
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-3">
          <span className="flex h-[34px] w-[34px] flex-shrink-0 items-center justify-center rounded-full border border-dashed border-gray-300 bg-gray-50 text-sm font-semibold text-gray-400">
            {group.token_display.charAt(0).toUpperCase()}
          </span>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-[0.9375rem] font-semibold text-gray-900">
                {group.token_display}
              </span>
              <span className="rounded-full bg-gray-100 px-1.5 py-0.5 text-[0.6875rem] font-semibold uppercase tracking-wide text-gray-500">
                from title · low confidence
              </span>
            </div>
            <button
              type="button"
              onClick={() => setOpen(o => !o)}
              className="mt-0.5 text-[0.8125rem] text-gray-500 hover:text-gray-700"
              aria-expanded={open}
            >
              Seen in {count} session title{count === 1 ? '' : 's'} · {open ? 'hide' : 'show'}{' '}
              evidence
            </button>
            {open && titles.length > 0 && (
              <ul className="mt-2 flex flex-col gap-1">
                {titles.map((title, i) => (
                  <li key={i} className="flex items-center gap-1.5 text-[0.8125rem] text-gray-600">
                    <MessageCircle className="h-3 w-3 text-gray-400" />
                    {title}
                  </li>
                ))}
              </ul>
            )}
            {open && titles.length === 0 && (
              <p className="mt-2 text-[0.8125rem] text-gray-400">
                No session titles available (source sessions may have been removed).
              </p>
            )}
          </div>
        </div>
        <div className="flex flex-shrink-0 items-center gap-2">
          <Button
            size="sm"
            variant="ghost"
            disabled={busy}
            onClick={() => onIgnore(group)}
            className="text-gray-500"
          >
            <Ban className="mr-1 h-3.5 w-3.5" />
            Not a person
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => onCreate(group)}>
            <UserPlus className="mr-1 h-3.5 w-3.5" />
            Create contact…
          </Button>
        </div>
      </div>
    </div>
  )
}
