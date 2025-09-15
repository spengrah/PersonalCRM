'use client'

import { useState } from 'react'
import { Clock, Zap, Play, Pause, RotateCcw } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { 
  useAcceleratedTime, 
  useTimeAcceleration, 
  ACCELERATION_PRESETS,
  createAccelerationSettings 
} from '@/hooks/use-accelerated-time'
import { clsx } from 'clsx'

interface TimeAccelerationWidgetProps {
  className?: string
  position?: 'top-right' | 'bottom-right' | 'bottom-left' | 'top-left'
}

export function TimeAccelerationWidget({ 
  className, 
  position = 'top-right' 
}: TimeAccelerationWidgetProps) {
  const [isExpanded, setIsExpanded] = useState(false)
  const { 
    currentTime, 
    isAccelerated, 
    accelerationFactor, 
    environment,
    isLoading 
  } = useAcceleratedTime()
  
  const setAcceleration = useTimeAcceleration()

  // Only show in testing environments
  if (environment !== 'testing' && environment !== 'test' && environment !== 'staging') {
    return null
  }

  const handleSetAcceleration = async (factor: number) => {
    try {
      await setAcceleration.mutateAsync(
        createAccelerationSettings(factor, factor > 1)
      )
      if (factor === 1) {
        setIsExpanded(false)
      }
    } catch (error) {
      console.error('Error setting acceleration:', error)
    }
  }

  const formatTime = (date: Date) => {
    return date.toLocaleString('en-US', {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    })
  }

  const getAccelerationLabel = (factor: number) => {
    if (factor === 1) return 'Normal'
    if (factor === 60) return 'Fast (1min = 1hr)'
    if (factor === 1440) return 'Very Fast (1min = 1day)'
    if (factor === 43200) return 'Ultra Fast (1min = 30days)'
    return `${factor}x`
  }

  const positionClasses = {
    'top-right': 'top-4 right-4',
    'bottom-right': 'bottom-4 right-4',
    'bottom-left': 'bottom-4 left-4',
    'top-left': 'top-4 left-4',
  }

  return (
    <div className={clsx(
      'fixed z-50 transition-all duration-300',
      positionClasses[position],
      className
    )}>
      {/* Collapsed State */}
      {!isExpanded && (
        <Button
          onClick={() => setIsExpanded(true)}
          variant={isAccelerated ? "default" : "outline"}
          size="sm"
          className={clsx(
            'shadow-lg border-2',
            isAccelerated 
              ? 'bg-blue-600 hover:bg-blue-700 border-blue-400 text-white' 
              : 'bg-white hover:bg-gray-50 border-gray-200'
          )}
        >
          <Clock className="w-4 h-4 mr-2" />
          {isAccelerated && <Zap className="w-3 h-3 mr-1 text-yellow-300" />}
          {formatTime(currentTime)}
          {isAccelerated && (
            <span className="ml-1 text-xs opacity-90">
              {accelerationFactor}x
            </span>
          )}
        </Button>
      )}

      {/* Expanded State */}
      {isExpanded && (
        <div className="bg-white rounded-lg shadow-2xl border-2 border-gray-200 p-4 min-w-[280px]">
          <div className="flex items-center justify-between mb-3">
            <div className="flex items-center space-x-2">
              <Clock className="w-5 h-5 text-gray-600" />
              <span className="font-semibold text-gray-900">Time Control</span>
              {isAccelerated && <Zap className="w-4 h-4 text-blue-500" />}
            </div>
            <Button
              onClick={() => setIsExpanded(false)}
              variant="ghost"
              size="sm"
              className="h-6 w-6 p-0"
            >
              ×
            </Button>
          </div>

          {/* Current Time Display */}
          <div className="mb-4 p-3 bg-gray-50 rounded-md">
            <div className="text-sm text-gray-600 mb-1">Current Time</div>
            <div className="font-mono text-lg font-semibold text-gray-900">
              {formatTime(currentTime)}
            </div>
            <div className="text-xs text-gray-500 mt-1">
              {isAccelerated 
                ? `Accelerated ${accelerationFactor}x (${environment})`
                : `Normal Speed (${environment})`
              }
            </div>
          </div>

          {/* Acceleration Controls */}
          <div className="space-y-2">
            <div className="text-sm font-medium text-gray-700 mb-2">
              Speed Control
            </div>
            
            <div className="grid grid-cols-2 gap-2">
              <Button
                onClick={() => handleSetAcceleration(ACCELERATION_PRESETS.NORMAL)}
                variant={accelerationFactor === ACCELERATION_PRESETS.NORMAL ? "default" : "outline"}
                size="sm"
                disabled={setAcceleration.isPending}
                className="justify-start"
              >
                <Play className="w-3 h-3 mr-1" />
                Normal
              </Button>

              <Button
                onClick={() => handleSetAcceleration(ACCELERATION_PRESETS.FAST)}
                variant={accelerationFactor === ACCELERATION_PRESETS.FAST ? "default" : "outline"}
                size="sm"
                disabled={setAcceleration.isPending}
                className="justify-start"
              >
                <Zap className="w-3 h-3 mr-1" />
                Fast
              </Button>

              <Button
                onClick={() => handleSetAcceleration(ACCELERATION_PRESETS.VERY_FAST)}
                variant={accelerationFactor === ACCELERATION_PRESETS.VERY_FAST ? "default" : "outline"}
                size="sm"
                disabled={setAcceleration.isPending}
                className="justify-start"
              >
                <Zap className="w-3 h-3 mr-1" />
                Very Fast
              </Button>

              <Button
                onClick={() => handleSetAcceleration(ACCELERATION_PRESETS.ULTRA_FAST)}
                variant={accelerationFactor === ACCELERATION_PRESETS.ULTRA_FAST ? "default" : "outline"}
                size="sm"
                disabled={setAcceleration.isPending}
                className="justify-start"
              >
                <Zap className="w-3 h-3 mr-1" />
                Ultra Fast
              </Button>
            </div>
          </div>

          {/* Status */}
          {setAcceleration.isPending && (
            <div className="mt-3 text-xs text-blue-600 text-center">
              Updating time acceleration...
            </div>
          )}

          {isAccelerated && (
            <div className="mt-3 p-2 bg-blue-50 border border-blue-200 rounded text-xs text-blue-800">
              <strong>Testing Mode:</strong> Time is running {accelerationFactor}x faster than normal. 
              Perfect for testing birthday calculations and reminder cadences!
            </div>
          )}
        </div>
      )}
    </div>
  )
}

