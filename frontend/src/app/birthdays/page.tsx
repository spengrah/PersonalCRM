'use client'

import { Calendar, Cake, Gift, Users } from 'lucide-react'
import Link from 'next/link'
import { Navigation } from '@/components/layout/navigation'
import { useContacts } from '@/hooks/use-contacts'
import { useAcceleratedTime } from '@/hooks/use-accelerated-time'
import { isPlaceholderBirthday, parseDateOnly } from '@/lib/utils'
import type { Contact } from '@/types/contact'

// Birthday data with calculated fields
interface BirthdayInfo {
  contact: Contact
  birthday: Date
  dayOfWeek: string
  monthDay: string
  ageThisYear?: number
  daysUntil: number
  isPastThisYear: boolean
}

// Calculate age turning this year
function calculateAgeThisYear(birthday: Date, year: number): number {
  return year - birthday.getFullYear()
}

function startOfLocalDay(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate())
}

function birthdayInYear(birthday: Date, year: number): Date {
  return new Date(year, birthday.getMonth(), birthday.getDate())
}

function getNextBirthday(birthday: Date, currentTime: Date): Date {
  const currentDay = startOfLocalDay(currentTime)
  const currentYear = currentDay.getFullYear()
  const birthdayThisYear = birthdayInYear(birthday, currentYear)

  if (birthdayThisYear < currentDay) {
    return birthdayInYear(birthday, currentYear + 1)
  }

  return birthdayThisYear
}

// Calculate days until next birthday
function calculateDaysUntilBirthday(birthday: Date, currentTime: Date): number {
  const currentDay = startOfLocalDay(currentTime)
  const nextBirthday = getNextBirthday(birthday, currentTime)

  // Calculate difference in days
  const diffTime = nextBirthday.getTime() - currentDay.getTime()
  return Math.ceil(diffTime / (1000 * 60 * 60 * 24))
}

// Check if birthday already passed this year
function isBirthdayPastThisYear(birthday: Date, currentTime: Date): boolean {
  const currentDay = startOfLocalDay(currentTime)
  const birthdayThisYear = birthdayInYear(birthday, currentDay.getFullYear())

  return birthdayThisYear < currentDay
}

// Process contacts to extract birthday information
function processBirthdayContacts(contacts: Contact[], currentTime: Date): BirthdayInfo[] {
  return contacts
    .filter(contact => contact.birthday) // Only contacts with birthdays
    .map(contact => {
      const birthday = parseDateOnly(contact.birthday)
      if (!birthday) return null

      const daysUntil = calculateDaysUntilBirthday(birthday, currentTime)
      const nextBirthday = getNextBirthday(birthday, currentTime)
      const hasPlaceholderYear = isPlaceholderBirthday(contact.birthday)

      return {
        contact,
        birthday: nextBirthday,
        dayOfWeek: nextBirthday.toLocaleDateString('en-US', { weekday: 'long' }),
        monthDay: nextBirthday.toLocaleDateString('en-US', { month: 'long', day: 'numeric' }),
        ageThisYear: hasPlaceholderYear
          ? undefined
          : calculateAgeThisYear(birthday, currentTime.getFullYear()),
        daysUntil,
        isPastThisYear: isBirthdayPastThisYear(birthday, currentTime),
      }
    })
    .filter(info => info !== null)
    .sort((a, b) => {
      // Sort by days until birthday (closest first)
      // Past birthdays this year go to end
      if (a!.isPastThisYear && !b!.isPastThisYear) return 1
      if (!a!.isPastThisYear && b!.isPastThisYear) return -1
      return a!.daysUntil - b!.daysUntil
    }) as BirthdayInfo[]
}

// Birthday card component
function BirthdayCard({ birthdayInfo }: { birthdayInfo: BirthdayInfo }) {
  const { contact, dayOfWeek, monthDay, ageThisYear, daysUntil, isPastThisYear } = birthdayInfo

  // Special styling for birthdays that are today or very soon
  const isToday = daysUntil === 0
  const isThisWeek = daysUntil <= 7 && daysUntil > 0
  const isPastDue = isPastThisYear

  return (
    <div
      data-testid="birthday-card"
      className={`
      bg-white rounded-lg shadow-sm border p-4 hover:shadow-md transition-shadow
      ${isToday ? 'border-pink-300 bg-pink-50' : ''}
      ${isThisWeek ? 'border-yellow-300 bg-yellow-50' : ''}
      ${isPastDue ? 'border-gray-200 bg-gray-50' : ''}
    `}
    >
      <div className="flex items-center justify-between">
        <div className="flex items-center space-x-3">
          <div
            className={`
            p-2 rounded-full 
            ${isToday ? 'bg-pink-100 text-pink-600' : ''}
            ${isThisWeek ? 'bg-yellow-100 text-yellow-600' : ''}
            ${isPastDue ? 'bg-gray-100 text-gray-500' : ''}
            ${!isToday && !isThisWeek && !isPastDue ? 'bg-blue-100 text-blue-600' : ''}
          `}
          >
            {isToday ? <Gift className="w-5 h-5" /> : <Cake className="w-5 h-5" />}
          </div>

          <div>
            <h3 className="font-medium text-gray-900">
              <Link
                href={`/contacts/${contact.id}`}
                className="hover:text-blue-600 hover:underline"
              >
                {contact.full_name}
              </Link>
            </h3>
            <p className="text-sm text-gray-600">
              {dayOfWeek}, {monthDay}
            </p>
          </div>
        </div>

        <div className="text-right">
          {ageThisYear !== undefined && (
            <p className="text-lg font-semibold text-gray-900">
              {isPastDue ? `Turned ${ageThisYear}` : `Turning ${ageThisYear}`}
            </p>
          )}
          <p
            className={`text-sm ${
              isToday
                ? 'text-pink-600 font-semibold'
                : isThisWeek
                  ? 'text-yellow-600'
                  : isPastDue
                    ? 'text-gray-500'
                    : 'text-gray-600'
            }`}
          >
            {isToday
              ? '🎉 Today!'
              : isPastDue
                ? `${365 - Math.abs(daysUntil)} days ago`
                : `${daysUntil} day${daysUntil === 1 ? '' : 's'}`}
          </p>
        </div>
      </div>
    </div>
  )
}

export default function BirthdaysPage() {
  const { currentTime } = useAcceleratedTime()
  const { data: contactsData, isLoading, error } = useContacts({ limit: 1000 })

  // Note: currentTime from useAcceleratedTime automatically updates and handles focus/blur
  // No need for manual time management anymore

  // Process birthday data with accelerated time
  const birthdayInfos = contactsData
    ? processBirthdayContacts(contactsData.contacts, currentTime)
    : []

  const todaysBirthdays = birthdayInfos.filter(info => info.daysUntil === 0)
  const upcomingBirthdays = birthdayInfos.filter(info => info.daysUntil > 0 && !info.isPastThisYear)
  const pastBirthdays = birthdayInfos.filter(info => info.isPastThisYear)

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-4xl mx-auto py-6 px-4 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="mb-8">
          <div className="flex items-center space-x-3 mb-2">
            <Calendar className="w-8 h-8 text-blue-600" />
            <h1 className="text-3xl font-bold text-gray-900">Birthday Tracker</h1>
          </div>

          {/* Current Date */}
          <div className="bg-white rounded-lg shadow-sm border p-4 mb-6">
            <div className="flex items-center justify-between">
              <div>
                <p className="text-lg font-semibold text-gray-900">
                  {currentTime.toLocaleDateString('en-US', {
                    weekday: 'long',
                    year: 'numeric',
                    month: 'long',
                    day: 'numeric',
                  })}
                </p>
                <p className="text-sm text-gray-600">
                  {currentTime.toLocaleTimeString('en-US', {
                    hour: 'numeric',
                    minute: '2-digit',
                    hour12: true,
                  })}
                </p>
              </div>
              <div className="text-right">
                <p className="text-sm text-gray-600">Total Contacts</p>
                <p className="text-2xl font-bold text-blue-600">{birthdayInfos.length}</p>
                <p className="text-xs text-gray-500">with birthdays</p>
              </div>
            </div>
          </div>
        </div>

        {/* Loading State */}
        {isLoading && (
          <div className="text-center py-8">
            <div className="animate-spin rounded-full h-12 w-12 border-b-2 border-blue-600 mx-auto"></div>
            <p className="text-gray-600 mt-4">Loading birthdays...</p>
          </div>
        )}

        {/* Error State */}
        {error && (
          <div className="bg-red-50 border border-red-200 rounded-lg p-4 text-center">
            <p className="text-red-700">Failed to load birthday information</p>
          </div>
        )}

        {/* Birthday Lists */}
        {!isLoading && !error && (
          <div className="space-y-8">
            {/* Today's Birthdays */}
            {todaysBirthdays.length > 0 && (
              <section>
                <h2 className="text-xl font-semibold text-gray-900 mb-4 flex items-center">
                  <Gift className="w-5 h-5 text-pink-600 mr-2" />
                  Today&apos;s Birthdays ({todaysBirthdays.length})
                </h2>
                <div className="space-y-3">
                  {todaysBirthdays.map(birthdayInfo => (
                    <BirthdayCard key={birthdayInfo.contact.id} birthdayInfo={birthdayInfo} />
                  ))}
                </div>
              </section>
            )}

            {/* Upcoming Birthdays */}
            {upcomingBirthdays.length > 0 && (
              <section>
                <h2 className="text-xl font-semibold text-gray-900 mb-4 flex items-center">
                  <Cake className="w-5 h-5 text-blue-600 mr-2" />
                  Upcoming Birthdays ({upcomingBirthdays.length})
                </h2>
                <div className="space-y-3">
                  {upcomingBirthdays.map(birthdayInfo => (
                    <BirthdayCard key={birthdayInfo.contact.id} birthdayInfo={birthdayInfo} />
                  ))}
                </div>
              </section>
            )}

            {/* Past Birthdays This Year */}
            {pastBirthdays.length > 0 && (
              <section>
                <h2 className="text-xl font-semibold text-gray-900 mb-4 flex items-center">
                  <Users className="w-5 h-5 text-gray-600 mr-2" />
                  Already Celebrated This Year ({pastBirthdays.length})
                </h2>
                <div className="space-y-3">
                  {pastBirthdays.map(birthdayInfo => (
                    <BirthdayCard key={birthdayInfo.contact.id} birthdayInfo={birthdayInfo} />
                  ))}
                </div>
              </section>
            )}

            {/* Empty State */}
            {birthdayInfos.length === 0 && (
              <div className="text-center py-12">
                <Cake className="w-16 h-16 text-gray-400 mx-auto mb-4" />
                <h3 className="text-lg font-medium text-gray-900 mb-2">No Birthday Information</h3>
                <p className="text-gray-600 mb-4">No contacts have birthday information yet.</p>
                <p className="text-sm text-gray-500">
                  Add birthdays to your contacts to see them here!
                </p>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}
