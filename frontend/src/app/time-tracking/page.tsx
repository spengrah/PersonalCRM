'use client'

import { useState, useEffect } from 'react'
import { Navigation } from '@/components/layout/navigation'
import { Button } from '@/components/ui/button'
import {
  useTimeEntries,
  useRunningTimeEntry,
  useTimeEntryStats,
  useCreateTimeEntry,
  useUpdateTimeEntry,
  useDeleteTimeEntry,
  timeEntryKeys,
} from '@/hooks/use-time-entries'
import { useQueryClient } from '@tanstack/react-query'
import { Play, Square, Trash2, Clock as ClockIcon, ChevronLeft, ChevronRight } from 'lucide-react'
import type { CreateTimeEntryRequest, TimeEntry } from '@/types/time-entry'

function formatDuration(minutes: number): string {
  if (minutes === 0) {
    return '<1m'
  }
  const hours = Math.floor(minutes / 60)
  const mins = minutes % 60
  if (hours > 0) {
    return `${hours}h ${mins}m`
  }
  return `${mins}m`
}

function formatHours(minutes: number): string {
  if (minutes === 0) {
    return '<0.01h'
  }
  const hours = (minutes / 60).toFixed(2)
  return `${hours}h`
}

function formatDate(date: Date): string {
  return date.toLocaleDateString('en-US', {
    weekday: 'long',
    year: 'numeric',
    month: 'long',
    day: 'numeric',
  })
}

function getDateKey(date: Date): string {
  return date.toISOString().split('T')[0]
}

function groupEntriesByDay(entries: TimeEntry[]) {
  const grouped: Record<string, TimeEntry[]> = {}

  entries.forEach(entry => {
    // Check end_time exists and duration_minutes is a number (including 0)
    if (entry.end_time && entry.duration_minutes !== undefined && entry.duration_minutes !== null) {
      const date = new Date(entry.start_time)
      const dateKey = getDateKey(date)

      if (!grouped[dateKey]) {
        grouped[dateKey] = []
      }
      grouped[dateKey].push(entry)
    }
  })

  // Sort entries within each day by start time
  Object.keys(grouped).forEach(key => {
    grouped[key].sort((a, b) => new Date(b.start_time).getTime() - new Date(a.start_time).getTime())
  })

  return grouped
}

function formatElapsedTime(startTime: string): string {
  const start = new Date(startTime)
  const now = new Date()
  const diffMs = now.getTime() - start.getTime()
  const totalSeconds = Math.floor(diffMs / 1000)
  const hours = Math.floor(totalSeconds / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = totalSeconds % 60

  if (hours > 0) {
    return `${hours.toString().padStart(2, '0')}:${minutes.toString().padStart(2, '0')}:${seconds.toString().padStart(2, '0')}`
  }
  return `${minutes.toString().padStart(2, '0')}:${seconds.toString().padStart(2, '0')}`
}

function TimerDisplay({ startTime }: { startTime: string }) {
  const [elapsed, setElapsed] = useState(formatElapsedTime(startTime))

  useEffect(() => {
    const interval = setInterval(() => {
      setElapsed(formatElapsedTime(startTime))
    }, 1000)

    return () => clearInterval(interval)
  }, [startTime])

  return <span className="text-3xl font-mono font-bold text-blue-600">{elapsed}</span>
}

function TimerForm({
  onSubmit,
  onCancel,
}: {
  onSubmit: (data: { description: string; project?: string }) => void
  onCancel: () => void
}) {
  const [description, setDescription] = useState('')
  const [project, setProject] = useState('')

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (description.trim()) {
      onSubmit({ description: description.trim(), project: project.trim() || undefined })
      setDescription('')
      setProject('')
    }
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      <div>
        <label htmlFor="timer-description" className="block text-sm font-medium text-gray-900 mb-1">
          Description *
        </label>
        <input
          type="text"
          id="timer-description"
          value={description}
          onChange={e => setDescription(e.target.value)}
          className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900 placeholder:text-gray-500"
          placeholder="What are you working on?"
          required
        />
      </div>
      <div>
        <label htmlFor="timer-project" className="block text-sm font-medium text-gray-900 mb-1">
          Project (optional)
        </label>
        <input
          type="text"
          id="timer-project"
          value={project}
          onChange={e => setProject(e.target.value)}
          className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500"
          placeholder="Project name"
        />
      </div>
      <div className="flex space-x-2">
        <Button type="submit" className="flex-1">
          Start Timer
        </Button>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  )
}

function ManualEntryForm({
  onSubmit,
  onCancel,
}: {
  onSubmit: (data: CreateTimeEntryRequest) => void
  onCancel: () => void
}) {
  const [description, setDescription] = useState('')
  const [project, setProject] = useState('')
  const [startDate, setStartDate] = useState('')
  const [startTime, setStartTime] = useState('')
  const [endDate, setEndDate] = useState('')
  const [endTime, setEndTime] = useState('')
  const [durationHours, setDurationHours] = useState('')
  const [durationMinutes, setDurationMinutes] = useState('')
  const [useDuration, setUseDuration] = useState(false)

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!description.trim()) {
      alert('Description is required')
      return
    }

    let endTimeISO: string | undefined
    let durationMinutes: number | undefined

    // Parse start time
    if (!startDate || !startTime) {
      alert('Start date and time are required')
      return
    }
    const startTimeISO = new Date(`${startDate}T${startTime}`).toISOString()

    // Parse end time or duration
    if (useDuration) {
      const hours = parseInt(durationHours) || 0
      const mins = parseInt(durationMinutes) || 0
      if (hours === 0 && mins === 0) {
        alert('Please enter a duration')
        return
      }
      durationMinutes = hours * 60 + mins
      const start = new Date(startTimeISO)
      const end = new Date(start.getTime() + durationMinutes * 60000)
      endTimeISO = end.toISOString()
    } else {
      if (!endDate || !endTime) {
        alert('End date and time are required when not using duration')
        return
      }
      endTimeISO = new Date(`${endDate}T${endTime}`).toISOString()
      const start = new Date(startTimeISO)
      const end = new Date(endTimeISO)
      durationMinutes = Math.floor((end.getTime() - start.getTime()) / 60000)
    }

    onSubmit({
      description: description.trim(),
      project: project.trim() || undefined,
      start_time: startTimeISO,
      end_time: endTimeISO,
      duration_minutes: durationMinutes,
    })

    // Reset form
    setDescription('')
    setProject('')
    setStartDate('')
    setStartTime('')
    setEndDate('')
    setEndTime('')
    setDurationHours('')
    setDurationMinutes('')
    setUseDuration(false)
  }

  // Set default times to now
  useEffect(() => {
    const now = new Date()
    const dateStr = now.toISOString().split('T')[0]
    const timeStr = now.toTimeString().slice(0, 5)

    if (!startDate) setStartDate(dateStr)
    if (!startTime) setStartTime(timeStr)
    if (!endDate) setEndDate(dateStr)
    if (!endTime) setEndTime(timeStr)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      <div>
        <label
          htmlFor="manual-description"
          className="block text-sm font-medium text-gray-900 mb-1"
        >
          Description *
        </label>
        <input
          type="text"
          id="manual-description"
          value={description}
          onChange={e => setDescription(e.target.value)}
          className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900 placeholder:text-gray-500"
          placeholder="What did you work on?"
          required
        />
      </div>
      <div>
        <label htmlFor="manual-project" className="block text-sm font-medium text-gray-900 mb-1">
          Project (optional)
        </label>
        <input
          type="text"
          id="manual-project"
          value={project}
          onChange={e => setProject(e.target.value)}
          className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900 placeholder:text-gray-500"
          placeholder="Project name"
        />
      </div>

      <div className="grid grid-cols-2 gap-4">
        <div>
          <label htmlFor="start-date" className="block text-sm font-medium text-gray-900 mb-1">
            Start Date *
          </label>
          <input
            type="date"
            id="start-date"
            value={startDate}
            onChange={e => setStartDate(e.target.value)}
            className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900"
            required
          />
        </div>
        <div>
          <label htmlFor="start-time" className="block text-sm font-medium text-gray-900 mb-1">
            Start Time *
          </label>
          <input
            type="time"
            id="start-time"
            value={startTime}
            onChange={e => setStartTime(e.target.value)}
            className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900"
            required
          />
        </div>
      </div>

      <div className="flex items-center space-x-2">
        <input
          type="checkbox"
          id="use-duration"
          checked={useDuration}
          onChange={e => setUseDuration(e.target.checked)}
          className="h-4 w-4 text-blue-600 focus:ring-blue-500 border-gray-300 rounded"
        />
        <label htmlFor="use-duration" className="text-sm font-medium text-gray-900">
          Use duration instead of end time
        </label>
      </div>

      {useDuration ? (
        <div className="grid grid-cols-2 gap-4">
          <div>
            <label
              htmlFor="duration-hours"
              className="block text-sm font-medium text-gray-900 mb-1"
            >
              Hours
            </label>
            <input
              type="number"
              id="duration-hours"
              min="0"
              value={durationHours}
              onChange={e => setDurationHours(e.target.value)}
              className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900 placeholder:text-gray-500"
              placeholder="0"
            />
          </div>
          <div>
            <label
              htmlFor="duration-minutes"
              className="block text-sm font-medium text-gray-900 mb-1"
            >
              Minutes
            </label>
            <input
              type="number"
              id="duration-minutes"
              min="0"
              max="59"
              value={durationMinutes}
              onChange={e => setDurationMinutes(e.target.value)}
              className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900 placeholder:text-gray-500"
              placeholder="0"
            />
          </div>
        </div>
      ) : (
        <div className="grid grid-cols-2 gap-4">
          <div>
            <label htmlFor="end-date" className="block text-sm font-medium text-gray-900 mb-1">
              End Date *
            </label>
            <input
              type="date"
              id="end-date"
              value={endDate}
              onChange={e => setEndDate(e.target.value)}
              className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900"
              required={!useDuration}
            />
          </div>
          <div>
            <label htmlFor="end-time" className="block text-sm font-medium text-gray-900 mb-1">
              End Time *
            </label>
            <input
              type="time"
              id="end-time"
              value={endTime}
              onChange={e => setEndTime(e.target.value)}
              className="w-full px-3 py-2 border border-gray-300 rounded-md shadow-sm focus:outline-none focus:ring-blue-500 focus:border-blue-500 text-gray-900"
              required={!useDuration}
            />
          </div>
        </div>
      )}

      <div className="flex space-x-2">
        <Button type="submit" className="flex-1">
          Add Time Entry
        </Button>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  )
}

function StatsCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-white rounded-lg shadow p-4">
      <div className="text-sm text-gray-900 mb-1">{label}</div>
      <div className="text-2xl font-semibold text-gray-900">{value}</div>
    </div>
  )
}

export default function TimeTrackingPage() {
  const [showTimerForm, setShowTimerForm] = useState(false)
  const [showManualForm, setShowManualForm] = useState(false)
  const [selectedDate, setSelectedDate] = useState(new Date())
  const queryClient = useQueryClient()
  const { data: runningEntry } = useRunningTimeEntry()
  const { data: entries, isLoading: isLoadingEntries } = useTimeEntries({ limit: 100 })
  const { data: stats, isLoading: isLoadingStats } = useTimeEntryStats()
  const createMutation = useCreateTimeEntry()
  const updateMutation = useUpdateTimeEntry()
  const deleteMutation = useDeleteTimeEntry()

  const handleStartTimer = async (data: { description: string; project?: string }) => {
    try {
      await createMutation.mutateAsync({
        description: data.description,
        project: data.project,
        start_time: new Date().toISOString(),
      })
      setShowTimerForm(false)
    } catch (error) {
      console.error('Failed to start timer:', error)
    }
  }

  const handleAddManualEntry = async (data: CreateTimeEntryRequest) => {
    try {
      await createMutation.mutateAsync(data)
      setShowManualForm(false)
    } catch (error) {
      console.error('Failed to add time entry:', error)
    }
  }

  const handleStopTimer = async () => {
    if (!runningEntry) return

    try {
      const endTime = new Date()
      const startTime = new Date(runningEntry.start_time)
      const durationMinutes = Math.floor((endTime.getTime() - startTime.getTime()) / 60000)

      await updateMutation.mutateAsync({
        id: runningEntry.id,
        data: {
          end_time: endTime.toISOString(),
          duration_minutes: durationMinutes,
        },
      })
      // Force refetch of running entry to clear it
      queryClient.invalidateQueries({ queryKey: timeEntryKeys.running() })
    } catch (error) {
      console.error('Failed to stop timer:', error)
      alert('Failed to stop timer. Please try again.')
    }
  }

  const handleDeleteEntry = async (id: string) => {
    if (confirm('Are you sure you want to delete this time entry?')) {
      try {
        await deleteMutation.mutateAsync(id)
      } catch (error) {
        console.error('Failed to delete entry:', error)
      }
    }
  }

  // Group entries by day
  const entriesByDay = entries ? groupEntriesByDay(entries) : {}
  const dayKeys = Object.keys(entriesByDay).sort().reverse() // Most recent first

  // Get entries for selected day
  const selectedDateKey = getDateKey(selectedDate)
  const dayEntries = entriesByDay[selectedDateKey] || []

  // Calculate total hours for the day
  const dayTotalMinutes = dayEntries.reduce((sum, entry) => sum + (entry.duration_minutes || 0), 0)
  const dayTotalHours = (dayTotalMinutes / 60).toFixed(2)

  // Navigate to previous/next day with entries
  // dayKeys is sorted reverse (most recent first): [2024-11-14, 2024-11-13, 2024-11-12, ...]
  // So index 0 = most recent, higher index = older days
  const navigateDay = (direction: 'prev' | 'next') => {
    if (dayKeys.length === 0) return

    const currentIndex = dayKeys.indexOf(selectedDateKey)

    if (currentIndex === -1) {
      // Current day has no entries, find the closest day with entries
      const today = new Date(selectedDate)
      today.setHours(0, 0, 0, 0)
      const todayKey = getDateKey(today)

      // Find where today would fit in the sorted array
      // dayKeys is reverse sorted, so we need to find the first key that's older than today
      let insertIndex = dayKeys.findIndex(key => key < todayKey)
      if (insertIndex === -1) insertIndex = dayKeys.length

      if (direction === 'prev') {
        // Go to older day (higher index in reverse-sorted array)
        if (insertIndex < dayKeys.length) {
          setSelectedDate(new Date(dayKeys[insertIndex] + 'T00:00:00'))
        }
      } else {
        // Go to newer day (lower index in reverse-sorted array)
        if (insertIndex > 0) {
          setSelectedDate(new Date(dayKeys[insertIndex - 1] + 'T00:00:00'))
        }
      }
    } else {
      // Current day has entries, navigate to adjacent day with entries
      if (direction === 'prev') {
        // Go to older day (higher index)
        if (currentIndex < dayKeys.length - 1) {
          setSelectedDate(new Date(dayKeys[currentIndex + 1] + 'T00:00:00'))
        }
      } else {
        // Go to newer day (lower index)
        if (currentIndex > 0) {
          setSelectedDate(new Date(dayKeys[currentIndex - 1] + 'T00:00:00'))
        }
      }
    }
  }

  // Check if we can navigate in each direction
  const canNavigatePrev = () => {
    if (dayKeys.length === 0) return false
    const currentIndex = dayKeys.indexOf(selectedDateKey)
    if (currentIndex === -1) {
      const today = new Date(selectedDate)
      today.setHours(0, 0, 0, 0)
      const todayKey = getDateKey(today)
      const insertIndex = dayKeys.findIndex(key => key < todayKey)
      return insertIndex !== -1 && insertIndex < dayKeys.length
    }
    return currentIndex < dayKeys.length - 1
  }

  const canNavigateNext = () => {
    if (dayKeys.length === 0) return false
    const currentIndex = dayKeys.indexOf(selectedDateKey)
    if (currentIndex === -1) {
      const today = new Date(selectedDate)
      today.setHours(0, 0, 0, 0)
      const todayKey = getDateKey(today)
      const insertIndex = dayKeys.findIndex(key => key < todayKey)
      return insertIndex > 0
    }
    return currentIndex > 0
  }

  // Navigate to today
  const navigateToToday = () => {
    const today = new Date()
    today.setHours(0, 0, 0, 0)
    setSelectedDate(today)
  }

  // Check if selected date is today
  const isToday = () => {
    const today = new Date()
    today.setHours(0, 0, 0, 0)
    return getDateKey(today) === selectedDateKey
  }

  // Initialize to most recent day with entries if current day has none
  useEffect(() => {
    if (dayKeys.length > 0 && dayEntries.length === 0) {
      const today = new Date()
      today.setHours(0, 0, 0, 0)
      const todayKey = getDateKey(today)

      // If selected date is today and today has no entries, go to most recent day with entries
      if (selectedDateKey === todayKey) {
        const mostRecentDay = dayKeys[0] // First key is most recent
        setSelectedDate(new Date(mostRecentDay))
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dayKeys.length])

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-7xl mx-auto py-6 sm:px-6 lg:px-8">
        <div className="px-4 py-6 sm:px-0">
          <div className="mb-6">
            <h1 className="text-3xl font-bold text-gray-900 mb-2">Time Tracking</h1>
            <p className="text-gray-900">Track your time spent on activities and projects</p>
          </div>

          {/* Stats Cards */}
          {!isLoadingStats && stats && (
            <div className="grid grid-cols-1 md:grid-cols-4 gap-4 mb-6">
              <StatsCard label="Total Time" value={formatDuration(stats.total_minutes)} />
              <StatsCard label="Today" value={formatDuration(stats.today_minutes)} />
              <StatsCard label="This Week" value={formatDuration(stats.week_minutes)} />
              <StatsCard label="This Month" value={formatDuration(stats.month_minutes)} />
            </div>
          )}

          {/* Running Timer */}
          {runningEntry ? (
            <div className="bg-white rounded-lg shadow p-6 mb-6">
              <div className="flex items-center justify-between">
                <div>
                  <div className="flex items-center space-x-2 mb-2">
                    <div className="w-3 h-3 bg-red-500 rounded-full animate-pulse"></div>
                    <span className="text-sm font-medium text-gray-900">Timer Running</span>
                  </div>
                  <div className="text-lg font-semibold text-gray-900 mb-1">
                    {runningEntry.description}
                  </div>
                  {runningEntry.project && (
                    <div className="text-sm text-gray-900">{runningEntry.project}</div>
                  )}
                </div>
                <div className="text-right">
                  <TimerDisplay startTime={runningEntry.start_time} />
                  <div className="mt-2">
                    <Button
                      onClick={handleStopTimer}
                      variant="danger"
                      size="sm"
                      disabled={updateMutation.isPending}
                      loading={updateMutation.isPending}
                    >
                      <Square className="w-4 h-4 mr-2" />
                      Stop
                    </Button>
                  </div>
                </div>
              </div>
            </div>
          ) : (
            <div className="bg-white rounded-lg shadow p-6 mb-6">
              {showTimerForm ? (
                <TimerForm onSubmit={handleStartTimer} onCancel={() => setShowTimerForm(false)} />
              ) : showManualForm ? (
                <ManualEntryForm
                  onSubmit={handleAddManualEntry}
                  onCancel={() => setShowManualForm(false)}
                />
              ) : (
                <div className="space-y-3">
                  <Button onClick={() => setShowTimerForm(true)} className="w-full">
                    <Play className="w-4 h-4 mr-2" />
                    Start New Timer
                  </Button>
                  <Button
                    onClick={() => setShowManualForm(true)}
                    variant="outline"
                    className="w-full"
                  >
                    <ClockIcon className="w-4 h-4 mr-2" />
                    Add Manual Entry
                  </Button>
                </div>
              )}
            </div>
          )}

          {/* Time Entries Grid by Day */}
          <div className="bg-white rounded-lg shadow">
            <div className="px-6 py-4 border-b border-gray-200">
              <div className="flex items-center justify-between">
                <h2 className="text-lg font-semibold text-gray-900">Recent Events</h2>
                <div className="flex items-center space-x-4">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => navigateDay('prev')}
                    disabled={!canNavigatePrev()}
                    title="Previous day with entries"
                  >
                    <ChevronLeft className="w-4 h-4" />
                  </Button>
                  <div className="flex flex-col items-center min-w-[250px]">
                    <div className="text-sm font-medium text-gray-900">
                      {formatDate(selectedDate)}
                    </div>
                    {!isToday() && (
                      <button
                        onClick={navigateToToday}
                        className="text-xs text-blue-600 hover:text-blue-800 mt-1"
                      >
                        Go to Today
                      </button>
                    )}
                  </div>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => navigateDay('next')}
                    disabled={!canNavigateNext()}
                    title="Next day with entries"
                  >
                    <ChevronRight className="w-4 h-4" />
                  </Button>
                </div>
              </div>
            </div>
            {isLoadingEntries ? (
              <div className="p-6 text-center text-gray-900">Loading entries...</div>
            ) : dayKeys.length === 0 ? (
              <div className="p-6 text-center text-gray-900">
                <ClockIcon className="w-12 h-12 mx-auto mb-4 text-gray-700" />
                <p>No time entries yet. Start a timer to begin tracking!</p>
              </div>
            ) : dayEntries.length === 0 ? (
              <div className="p-6 text-center text-gray-900">
                <ClockIcon className="w-12 h-12 mx-auto mb-4 text-gray-400" />
                <p className="text-gray-900">No entries for {formatDate(selectedDate)}.</p>
                <p className="text-sm text-gray-600 mt-2">
                  Use the arrows to navigate to days with entries.
                </p>
              </div>
            ) : (
              <>
                <div className="overflow-x-auto">
                  <table className="w-full">
                    <thead className="bg-gray-50">
                      <tr>
                        <th className="px-4 md:px-6 py-3 text-left text-xs font-medium text-gray-900 uppercase tracking-wider">
                          Project
                        </th>
                        <th className="px-4 md:px-6 py-3 text-left text-xs font-medium text-gray-900 uppercase tracking-wider">
                          Description
                        </th>
                        <th className="px-4 md:px-6 py-3 text-right text-xs font-medium text-gray-900 uppercase tracking-wider">
                          Hours
                        </th>
                        <th className="px-4 md:px-6 py-3 text-right text-xs font-medium text-gray-900 uppercase tracking-wider">
                          Actions
                        </th>
                      </tr>
                    </thead>
                    <tbody className="bg-white divide-y divide-gray-200">
                      {dayEntries.map(entry => (
                        <tr key={entry.id} className="hover:bg-gray-50">
                          <td className="px-4 md:px-6 py-4">
                            <span className="text-sm text-gray-900">
                              {entry.project || <span className="text-gray-400">-</span>}
                            </span>
                          </td>
                          <td className="px-4 md:px-6 py-4">
                            <div className="text-sm font-medium text-gray-900">
                              {entry.description}
                            </div>
                            <div className="text-xs text-gray-500 mt-1">
                              {new Date(entry.start_time).toLocaleTimeString('en-US', {
                                hour: 'numeric',
                                minute: '2-digit',
                                hour12: true,
                              })}
                              {entry.end_time &&
                                ` - ${new Date(entry.end_time).toLocaleTimeString('en-US', {
                                  hour: 'numeric',
                                  minute: '2-digit',
                                  hour12: true,
                                })}`}
                            </div>
                          </td>
                          <td className="px-4 md:px-6 py-4 whitespace-nowrap text-right">
                            <span className="text-sm font-semibold text-gray-900">
                              {formatHours(entry.duration_minutes || 0)}
                            </span>
                          </td>
                          <td className="px-4 md:px-6 py-4 whitespace-nowrap text-right">
                            <Button
                              variant="ghost"
                              size="sm"
                              onClick={() => handleDeleteEntry(entry.id)}
                              title="Delete entry"
                            >
                              <Trash2 className="w-4 h-4 text-red-600" />
                            </Button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                    <tfoot className="bg-gray-100 border-t-2 border-gray-300">
                      <tr>
                        <td
                          colSpan={2}
                          className="px-4 md:px-6 py-4 text-sm font-bold text-gray-900"
                        >
                          Total for {formatDate(selectedDate)}
                        </td>
                        <td className="px-4 md:px-6 py-4 whitespace-nowrap text-right">
                          <span className="text-base font-bold text-gray-900">
                            {dayTotalHours}h
                          </span>
                        </td>
                        <td className="px-4 md:px-6 py-4"></td>
                      </tr>
                    </tfoot>
                  </table>
                </div>
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
