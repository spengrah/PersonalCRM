'use client'

import { Suspense } from 'react'
import Link from 'next/link'
import { useRouter, useSearchParams } from 'next/navigation'
import { clsx } from 'clsx'
import {
  InteractionsTab,
  interactionFiltersFrom,
  interactionsAddress,
} from '@/components/bench/interactions-tab'
import { Navigation } from '@/components/layout/navigation'
import { listFiltersFrom, threadsAddress } from '@/components/threads/address'
import { ReviewSwitch } from '@/components/threads/review-switch'
import { ThreadsList } from '@/components/threads/threads-list'

const TABS = [
  { key: 'interactions', label: 'Interactions', href: '/interactions' },
  { key: 'threads', label: 'Threads', href: '/interactions?tab=threads' },
] as const

function InteractionsPageContent() {
  const router = useRouter()
  const params = useSearchParams()
  // An address naming no tab, or one this page does not know, opens Interactions.
  const tab = params.get('tab') === 'threads' ? 'threads' : 'interactions'

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />
      <div className="max-w-7xl mx-auto py-6 px-4 sm:px-6 lg:px-8">
        <div className="mb-4 flex items-center justify-between gap-4">
          <h2 className="text-2xl font-bold leading-normal text-gray-900 sm:text-3xl sm:truncate">
            Interactions
          </h2>
          <ReviewSwitch />
        </div>
        <div
          role="tablist"
          aria-label="Interactions views"
          className="mb-4 flex gap-1 border-b border-gray-200"
        >
          {TABS.map(({ key, label, href }) => (
            <Link
              key={key}
              role="tab"
              aria-selected={tab === key}
              href={href}
              className={clsx(
                '-mb-px border-b-2 px-4 py-2 text-sm font-medium',
                tab === key
                  ? 'border-blue-600 text-blue-700'
                  : 'border-transparent text-gray-500 hover:text-gray-700'
              )}
            >
              {label}
            </Link>
          ))}
        </div>
        <section
          role="tabpanel"
          aria-label={tab === 'threads' ? 'Threads' : 'All interactions'}
          className="bg-white shadow sm:rounded-lg"
        >
          {tab === 'threads' ? (
            <ThreadsList
              filters={listFiltersFrom(params)}
              onChange={next => router.push(threadsAddress(next))}
            />
          ) : (
            <InteractionsTab
              filters={interactionFiltersFrom(params)}
              onChange={next => router.push(interactionsAddress(next))}
            />
          )}
        </section>
      </div>
    </div>
  )
}

// useSearchParams in a prerendered page needs a Suspense boundary.
export default function InteractionsPage() {
  return (
    <Suspense>
      <InteractionsPageContent />
    </Suspense>
  )
}
