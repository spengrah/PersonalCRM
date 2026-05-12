'use client'

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { macHostsApi } from '@/lib/mac-hosts-api'
import { macHostKeys } from '@/lib/query-keys'

/**
 * useMacHosts fetches the list of paired Mac hosts. Polled every 10s
 * while the page is open so heartbeat-staleness badges stay fresh
 * without manual refresh.
 */
export function useMacHosts() {
  return useQuery({
    queryKey: macHostKeys.list(),
    queryFn: () => macHostsApi.list(),
    refetchInterval: 10_000,
  })
}

/**
 * useMacHost fetches a single host detail.
 */
export function useMacHost(id: string) {
  return useQuery({
    queryKey: macHostKeys.detail(id),
    queryFn: () => macHostsApi.get(id),
    enabled: Boolean(id),
  })
}

/**
 * useDeleteMacHost revokes a Mac host. Invalidates the list query on
 * success so the row disappears from the UI immediately.
 */
export function useDeleteMacHost() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => macHostsApi.delete(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: macHostKeys.all })
    },
  })
}

/**
 * useCreatePairingToken mints a fresh pairing token. The returned token
 * is single-show; callers must surface it to the operator without
 * persisting locally.
 */
export function useCreatePairingToken() {
  return useMutation({
    mutationFn: () => macHostsApi.createPairingToken(),
  })
}
