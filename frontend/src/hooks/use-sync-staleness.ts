import { useQuery } from '@tanstack/react-query'
import { syncApi } from '@/lib/sync-api'
import { syncKeys } from '@/lib/query-keys'

// useSyncStaleness polls the sync-staleness watchdog's active-breach endpoint.
// The producer runs every 5 minutes on the Pi, so a 60s poll is plenty
// fresh and cheaper than the mac page's 10s precedent. Read-only: breaches
// have no client mutations, so there is no invalidation rule.
export function useSyncStaleness() {
  return useQuery({
    queryKey: syncKeys.staleness(),
    queryFn: () => syncApi.getStalenessBreaches(),
    staleTime: 30_000,
    refetchInterval: 60_000,
  })
}
