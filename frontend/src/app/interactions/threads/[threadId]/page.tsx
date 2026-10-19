'use client'

import { Suspense } from 'react'
import { useParams, useSearchParams } from 'next/navigation'
import { listFiltersFrom } from '@/components/threads/address'
import { ThreadPage } from '@/components/thread-page/thread-page'

function ThreadRoute() {
  const params = useParams<{ threadId: string }>()
  const search = useSearchParams()
  return <ThreadPage threadId={params.threadId} filters={listFiltersFrom(search)} />
}

// useSearchParams in a prerendered page needs a Suspense boundary.
export default function ThreadRoutePage() {
  return (
    <Suspense>
      <ThreadRoute />
    </Suspense>
  )
}
