import { useState, useEffect, useRef } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { systemApi } from '@/lib/system-api'
import { systemKeys } from '@/lib/query-keys'
import type { SetAccelerationRequest } from '@/types/system'

// Re-export systemKeys for backward compatibility
export { systemKeys }

/**
 * Hook for getting the current accelerated time from the backend
 * This ensures all components use the same accelerated time for testing
 *
 * Performance optimization: Only runs intervals when time acceleration is enabled.
 * When not accelerated, returns new Date() on each render (no polling).
 * Also pauses intervals when the browser tab is hidden.
 */
export function useAcceleratedTime() {
  const [localTime, setLocalTime] = useState(new Date())
  const [isPageVisible, setIsPageVisible] = useState(true)
  const intervalRef = useRef<NodeJS.Timeout | null>(null)

  const {
    data: systemTime,
    isLoading,
    error,
  } = useQuery({
    queryKey: systemKeys.time(),
    queryFn: systemApi.getSystemTime,
    staleTime: 1000 * 60 * 5, // 5 minutes when not accelerated
    // Only poll when time is accelerated (for dev/testing)
    // In production on Pi, time won't be accelerated, so no polling
    refetchInterval: query => {
      const data = query.state.data
      return data?.is_accelerated ? 1000 * 30 : false // 30s when accelerated, never otherwise
    },
    refetchOnWindowFocus: true,
  })

  // Track page visibility to pause intervals when tab is hidden
  useEffect(() => {
    const handleVisibilityChange = () => {
      setIsPageVisible(!document.hidden)
    }

    document.addEventListener('visibilitychange', handleVisibilityChange)
    return () => {
      document.removeEventListener('visibilitychange', handleVisibilityChange)
    }
  }, [])

  // Update local time based on system time and acceleration
  useEffect(() => {
    // Clear any existing interval
    if (intervalRef.current) {
      clearInterval(intervalRef.current)
      intervalRef.current = null
    }

    // Only run intervals when acceleration is enabled AND page is visible
    if (systemTime?.is_accelerated && isPageVisible) {
      const baseTime = new Date(systemTime.base_time)

      const updateAcceleratedTime = () => {
        // Calculate elapsed from baseTime (when acceleration started), not from when effect ran
        // This ensures time doesn't reset when navigating between pages
        const elapsed = Date.now() - baseTime.getTime()
        const acceleratedElapsed = elapsed * systemTime.acceleration_factor
        const acceleratedTime = new Date(baseTime.getTime() + acceleratedElapsed)
        setLocalTime(acceleratedTime)
      }

      // Update immediately
      updateAcceleratedTime()

      // Update every second for smooth time progression (only when accelerated)
      intervalRef.current = setInterval(updateAcceleratedTime, 1000)
    } else if (!systemTime?.is_accelerated) {
      // For normal time, just set current date once (no interval needed)
      // Components will get fresh time on re-render
      setLocalTime(new Date())
    }

    return () => {
      if (intervalRef.current) {
        clearInterval(intervalRef.current)
        intervalRef.current = null
      }
    }
  }, [systemTime, isPageVisible])

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (intervalRef.current) {
        clearInterval(intervalRef.current)
      }
    }
  }, [])

  // When not accelerated, return fresh Date() each time for accurate time
  const currentTime = systemTime?.is_accelerated ? localTime : new Date()

  return {
    currentTime,
    systemTime,
    isAccelerated: systemTime?.is_accelerated || false,
    accelerationFactor: systemTime?.acceleration_factor || 1,
    environment: systemTime?.environment || 'unknown',
    isLoading,
    error,
  }
}

/**
 * Hook for setting time acceleration (testing environments only)
 */
export function useTimeAcceleration() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (settings: SetAccelerationRequest) => systemApi.setTimeAcceleration(settings),
    onSuccess: () => {
      // Invalidate system time to refetch with new acceleration
      queryClient.invalidateQueries({ queryKey: systemKeys.time() })
    },
  })
}

/**
 * Preset acceleration factors for common testing scenarios
 */
export const ACCELERATION_PRESETS = {
  NORMAL: 1,
  FAST: 60, // 1 minute = 1 hour
  VERY_FAST: 1440, // 1 minute = 1 day
  ULTRA_FAST: 43200, // 1 minute = 30 days
} as const

/**
 * Helper to create acceleration settings
 */
export function createAccelerationSettings(factor: number): SetAccelerationRequest {
  return { factor }
}
