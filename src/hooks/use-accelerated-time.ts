import { useState, useEffect, useRef } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { systemApi } from '@/lib/system-api'
import type { SystemTimeResponse, SetAccelerationRequest } from '@/types/system'

// Query keys
export const systemKeys = {
  all: ['system'] as const,
  time: () => [...systemKeys.all, 'time'] as const,
}

/**
 * Hook for getting the current accelerated time from the backend
 * This ensures all components use the same accelerated time for testing
 */
export function useAcceleratedTime() {
  const [localTime, setLocalTime] = useState(new Date())
  const intervalRef = useRef<NodeJS.Timeout | null>(null)

  const { data: systemTime, isLoading, error } = useQuery({
    queryKey: systemKeys.time(),
    queryFn: systemApi.getSystemTime,
    refetchInterval: 30000, // Refetch every 30 seconds
    staleTime: 25000, // Consider stale after 25 seconds
  })

  // Update local time based on system time and acceleration
  useEffect(() => {
    if (!systemTime) return

    // Clear any existing interval
    if (intervalRef.current) {
      clearInterval(intervalRef.current)
    }

    if (systemTime.is_accelerated) {
      // For accelerated time, calculate the offset and update frequently
      const serverTime = new Date(systemTime.current_time)
      const baseTime = new Date(systemTime.base_time)
      const localBaseTime = Date.now()
      
      const updateAcceleratedTime = () => {
        const elapsed = Date.now() - localBaseTime
        const acceleratedElapsed = elapsed * systemTime.acceleration_factor
        const acceleratedTime = new Date(baseTime.getTime() + acceleratedElapsed)
        setLocalTime(acceleratedTime)
      }

      // Update immediately
      updateAcceleratedTime()

      // Update every second for smooth time progression
      intervalRef.current = setInterval(updateAcceleratedTime, 1000)
    } else {
      // For normal time, just use regular Date
      setLocalTime(new Date())
      intervalRef.current = setInterval(() => setLocalTime(new Date()), 1000)
    }

    return () => {
      if (intervalRef.current) {
        clearInterval(intervalRef.current)
      }
    }
  }, [systemTime])

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (intervalRef.current) {
        clearInterval(intervalRef.current)
      }
    }
  }, [])

  return {
    currentTime: localTime,
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
    mutationFn: (settings: SetAccelerationRequest) => 
      systemApi.setTimeAcceleration(settings),
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
  FAST: 60,          // 1 minute = 1 hour
  VERY_FAST: 1440,   // 1 minute = 1 day  
  ULTRA_FAST: 43200, // 1 minute = 30 days
} as const

/**
 * Helper to create acceleration settings with current time as base
 */
export function createAccelerationSettings(
  factor: number, 
  enabled: boolean = true
): SetAccelerationRequest {
  return {
    enabled,
    acceleration_factor: factor,
    base_time: new Date().toISOString(),
  }
}

