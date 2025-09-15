'use client'

import { useEffect, useState } from 'react'
import { Calendar, Cake, Gift, Users } from 'lucide-react'
import { Navigation } from '@/components/layout/navigation'
import { useContacts } from '@/hooks/use-contacts'
import { useAcceleratedTime } from '@/hooks/use-accelerated-time'
import type { Contact } from '@/types/contact'

// Birthday data with calculated fields
interface BirthdayInfo {
  contact: Contact
  birthday: Date
  dayOfWeek: string
  monthDay: string
  ageThisYear: number
  daysUntil: number
  isPastThisYear: boolean
  isNextYear?: boolean
}

// Calculate age turning this year
function calculateAgeThisYear(birthday: Date, currentTime: Date): number {
  const currentYear = currentTime.getFullYear()
  const birthdayThisYear = new Date(currentYear, birthday.getMonth(), birthday.getDate())
  
  // Age they turn this calendar year
  return currentYear - birthday.getFullYear()
}

// Calculate days until next birthday
function calculateDaysUntilBirthday(birthday: Date, currentTime: Date): number {
  const currentYear = currentTime.getFullYear()
  
  // Birthday this year
  let nextBirthday = new Date(currentYear, birthday.getMonth(), birthday.getDate())
  
  // If birthday already passed this year, calculate for next year
  if (nextBirthday < currentTime) {
    nextBirthday = new Date(currentYear + 1, birthday.getMonth(), birthday.getDate())
  }
  
  // Calculate difference in days
  const diffTime = nextBirthday.getTime() - currentTime.getTime()
  return Math.ceil(diffTime / (1000 * 60 * 60 * 24))
}

// Check if we should show next year's early birthdays (for gift planning)
function shouldShowNextYearBirthdays(currentTime: Date): boolean {
  const month = currentTime.getMonth() + 1 // getMonth() is 0-based
  const day = currentTime.getDate()
  
  // Show next year's Jan-Mar birthdays if we're in November or December
  return month >= 11 // November (11) or December (12)
}

// Get next year's early birthdays for gift planning
function getNextYearEarlyBirthdays(contacts: Contact[], currentTime: Date): BirthdayInfo[] {
  if (!shouldShowNextYearBirthdays(currentTime)) {
    return []
  }
  
  return contacts
    .filter(contact => contact.birthday)
    .map(contact => {
      const birthday = new Date(contact.birthday!)
      const birthdayMonth = birthday.getMonth() + 1
      
      // Only include Jan-Mar birthdays from next year
      if (birthdayMonth > 3) return null
      
      const nextYear = currentTime.getFullYear() + 1
      const nextYearBirthday = new Date(nextYear, birthday.getMonth(), birthday.getDate())
      const daysUntil = Math.ceil((nextYearBirthday.getTime() - currentTime.getTime()) / (1000 * 60 * 60 * 24))
      
      return {
        contact,
        birthday: nextYearBirthday,
        dayOfWeek: nextYearBirthday.toLocaleDateString('en-US', { weekday: 'long' }),
        monthDay: nextYearBirthday.toLocaleDateString('en-US', { month: 'long', day: 'numeric' }),
        ageThisYear: nextYear - birthday.getFullYear(),
        daysUntil,
        isPastThisYear: false,
        isNextYear: true
      }
    })
    .filter(info => info !== null) as (BirthdayInfo & { isNextYear: boolean })[]
}

// Check if birthday already passed this year
function isBirthdayPastThisYear(birthday: Date, currentTime: Date): boolean {
  const currentYear = currentTime.getFullYear()
  const birthdayThisYear = new Date(currentYear, birthday.getMonth(), birthday.getDate())
  
  return birthdayThisYear < currentTime
}

// Process contacts to extract birthday information
function processBirthdayContacts(contacts: Contact[], currentTime: Date): BirthdayInfo[] {
  return contacts
    .filter(contact => contact.birthday) // Only contacts with birthdays
    .map(contact => {
      const birthday = new Date(contact.birthday!)
      const daysUntil = calculateDaysUntilBirthday(birthday, currentTime)
      
      return {
        contact,
        birthday,
        dayOfWeek: birthday.toLocaleDateString('en-US', { weekday: 'long' }),
        monthDay: birthday.toLocaleDateString('en-US', { month: 'long', day: 'numeric' }),
        ageThisYear: calculateAgeThisYear(birthday, currentTime),
        daysUntil,
        isPastThisYear: isBirthdayPastThisYear(birthday, currentTime)
      }
    })
    .sort((a, b) => {
      // Sort by days until birthday (closest first)
      // Past birthdays this year go to end
      if (a.isPastThisYear && !b.isPastThisYear) return 1
      if (!a.isPastThisYear && b.isPastThisYear) return -1
      return a.daysUntil - b.daysUntil
    })
}

// Birthday card component
function BirthdayCard({ birthdayInfo }: { birthdayInfo: BirthdayInfo }) {
  const { contact, dayOfWeek, monthDay, ageThisYear, daysUntil, isPastThisYear, isNextYear } = birthdayInfo
  
  // Special styling for birthdays that are today or very soon
  const isToday = daysUntil === 0
  const isThisWeek = daysUntil <= 7 && daysUntil > 0
  const isPastDue = isPastThisYear
  const isGiftPlanningTime = isNextYear // Next year's early birthdays for gift planning
  
  return (
    <div className={`
      bg-white rounded-lg shadow-sm border p-4 hover:shadow-md transition-shadow
      ${isToday ? 'border-pink-300 bg-pink-50' : ''}
      ${isThisWeek ? 'border-yellow-300 bg-yellow-50' : ''}
      ${isPastDue ? 'border-gray-200 bg-gray-50' : ''}
      ${isGiftPlanningTime ? 'border-purple-300 bg-purple-50' : ''}
    `}>
      <div className="flex items-center justify-between">
        <div className="flex items-center space-x-3">
          <div className={`
            p-2 rounded-full 
            ${isToday ? 'bg-pink-100 text-pink-600' : ''}
            ${isThisWeek ? 'bg-yellow-100 text-yellow-600' : ''}
            ${isPastDue ? 'bg-gray-100 text-gray-500' : ''}
            ${isGiftPlanningTime ? 'bg-purple-100 text-purple-600' : ''}
            ${!isToday && !isThisWeek && !isPastDue && !isGiftPlanningTime ? 'bg-blue-100 text-blue-600' : ''}
          `}>
            {isToday ? <Gift className="w-5 h-5" /> : isGiftPlanningTime ? <Gift className="w-5 h-5" /> : <Cake className="w-5 h-5" />}
          </div>
          
          <div>
            <h3 className="font-medium text-gray-900">
              {contact.full_name}
            </h3>
            <p className="text-sm text-gray-600">
              {dayOfWeek}, {monthDay}
            </p>
          </div>
        </div>
        
        <div className="text-right">
          <p className="text-lg font-semibold text-gray-900">
            {isPastDue ? `Turned ${ageThisYear}` : `Turning ${ageThisYear}`}
            {isGiftPlanningTime && <span className="text-xs text-purple-600 ml-1">(next year)</span>}
          </p>
          <p className={`text-sm ${
            isToday ? 'text-pink-600 font-semibold' : 
            isThisWeek ? 'text-yellow-600' : 
            isPastDue ? 'text-gray-500' : 
            isGiftPlanningTime ? 'text-purple-600' : 'text-gray-600'
          }`}>
            {isToday 
              ? '🎉 Today!' 
              : isPastDue 
                ? `${365 - Math.abs(daysUntil)} days ago`
                : isGiftPlanningTime
                  ? `🎁 ${daysUntil} day${daysUntil === 1 ? '' : 's'} (gift planning)`
                  : `${daysUntil} day${daysUntil === 1 ? '' : 's'}`
            }
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
  const birthdayInfos = contactsData ? processBirthdayContacts(contactsData.contacts, currentTime) : []
  const nextYearEarlyBirthdays = contactsData ? getNextYearEarlyBirthdays(contactsData.contacts, currentTime) : []
  
  const todaysBirthdays = birthdayInfos.filter(info => info.daysUntil === 0)
  const upcomingBirthdays = birthdayInfos.filter(info => info.daysUntil > 0 && !info.isPastThisYear)
  const pastBirthdays = birthdayInfos.filter(info => info.isPastThisYear)
  const giftPlanningBirthdays = nextYearEarlyBirthdays
  
  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />
      
      <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
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
                    day: 'numeric' 
                  })}
                </p>
                <p className="text-sm text-gray-600">
                  {currentTime.toLocaleTimeString('en-US', { 
                    hour: 'numeric', 
                    minute: '2-digit',
                    hour12: true 
                  })}
                </p>
              </div>
              <div className="text-right">
                <p className="text-sm text-gray-600">Total Contacts</p>
                <p className="text-2xl font-bold text-blue-600">
                  {birthdayInfos.length}
                </p>
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
                  Today's Birthdays ({todaysBirthdays.length})
                </h2>
                <div className="space-y-3">
                  {todaysBirthdays.map(birthdayInfo => (
                    <BirthdayCard key={birthdayInfo.contact.id} birthdayInfo={birthdayInfo} />
                  ))}
                </div>
              </section>
            )}

            {/* Gift Planning (Next Year Early Birthdays) */}
            {giftPlanningBirthdays.length > 0 && (
              <section>
                <h2 className="text-xl font-semibold text-gray-900 mb-4 flex items-center">
                  <Gift className="w-5 h-5 text-purple-600 mr-2" />
                  Gift Planning - Early {currentTime.getFullYear() + 1} Birthdays ({giftPlanningBirthdays.length})
                </h2>
                <div className="bg-purple-50 border border-purple-200 rounded-lg p-3 mb-4">
                  <p className="text-sm text-purple-700">
                    🎁 These Jan-Mar birthdays are coming up next year. Plan ahead for gifts during holiday shopping!
                  </p>
                </div>
                <div className="space-y-3">
                  {giftPlanningBirthdays.map(birthdayInfo => (
                    <BirthdayCard key={`next-year-${birthdayInfo.contact.id}`} birthdayInfo={birthdayInfo} />
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
                <p className="text-gray-600 mb-4">
                  No contacts have birthday information yet.
                </p>
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
