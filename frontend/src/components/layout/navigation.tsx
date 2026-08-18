'use client'

import { useState } from 'react'
import Link from 'next/link'
import { usePathname } from 'next/navigation'
import { useIsMutating } from '@tanstack/react-query'
import { Users, Calendar, Settings, Cake, CloudDownload } from 'lucide-react'
import { clsx } from 'clsx'
import { TimeAccelerationWidget } from '@/components/ui/time-acceleration-widget'
import { useSyncStates, getAggregateSyncStatus, getSyncIconClasses } from '@/hooks/use-sync-states'
import { syncMutationKey } from '@/hooks/use-imports'

const navigation = [
  { name: 'Dashboard', href: '/dashboard', icon: Calendar },
  { name: 'Contacts', href: '/contacts', icon: Users },
  { name: 'Birthdays', href: '/birthdays', icon: Cake },
  { name: 'Imports', href: '/imports', icon: CloudDownload },
  { name: 'Settings', href: '/settings', icon: Settings },
]

export function Navigation() {
  const pathname = usePathname()
  const [isMobileMenuOpen, setIsMobileMenuOpen] = useState(false)
  const { data: syncStates } = useSyncStates()
  const isSyncMutating = useIsMutating({ mutationKey: syncMutationKey })

  // Show 'syncing' if a sync mutation is in flight (optimistic UI)
  // This is needed because the sync API is synchronous - the backend
  // doesn't return until sync completes, so we can't fetch the 'syncing'
  // status from the DB during the sync.
  const syncStatus = isSyncMutating > 0 ? 'syncing' : getAggregateSyncStatus(syncStates)

  return (
    <nav className="bg-white shadow-sm border-b sticky top-0 z-50">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <div className="flex justify-between h-16">
          <div className="flex">
            <div className="flex-shrink-0 flex items-center">
              <h1 className="text-xl font-semibold text-gray-900">Personal CRM</h1>
            </div>
            <div className="hidden sm:ml-6 sm:flex sm:space-x-8">
              {navigation.map(item => {
                const isActive = pathname.startsWith(item.href)
                const Icon = item.icon

                return (
                  <Link
                    key={item.name}
                    href={item.href}
                    aria-current={isActive ? 'page' : undefined}
                    className={clsx(
                      'inline-flex items-center px-1 pt-1 border-b-2 text-sm font-medium',
                      isActive
                        ? 'border-blue-500 text-gray-900'
                        : 'border-transparent text-gray-500 hover:text-gray-700 hover:border-gray-300'
                    )}
                  >
                    <Icon
                      className={clsx(
                        'w-4 h-4 mr-2',
                        item.name === 'Imports' && getSyncIconClasses(syncStatus)
                      )}
                    />
                    {item.name}
                  </Link>
                )
              })}
            </div>
          </div>

          <div className="flex items-center space-x-4">
            {/* Time Acceleration Widget */}
            <TimeAccelerationWidget position="top-right" />

            {/* Mobile menu button */}
            <div className="sm:hidden">
              <button
                type="button"
                aria-expanded={isMobileMenuOpen}
                aria-controls="mobile-menu"
                onClick={() => setIsMobileMenuOpen(open => !open)}
                className="inline-flex items-center justify-center p-2 rounded-md text-gray-400 hover:text-gray-500 hover:bg-gray-100"
              >
                <span className="sr-only">Open main menu</span>
                <svg
                  className="block h-6 w-6"
                  fill="none"
                  viewBox="0 0 24 24"
                  stroke="currentColor"
                >
                  <path
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    strokeWidth={2}
                    d={isMobileMenuOpen ? 'M6 18L18 6M6 6l12 12' : 'M4 6h16M4 12h16M4 18h16'}
                  />
                </svg>
              </button>
            </div>
          </div>
        </div>
      </div>

      {/* Mobile menu panel */}
      {isMobileMenuOpen && (
        <div id="mobile-menu" className="sm:hidden border-t border-gray-200">
          <div className="space-y-1 pb-3 pt-2">
            {navigation.map(item => {
              const isActive = pathname.startsWith(item.href)
              const Icon = item.icon

              return (
                <Link
                  key={item.name}
                  href={item.href}
                  aria-current={isActive ? 'page' : undefined}
                  onClick={() => setIsMobileMenuOpen(false)}
                  className={clsx(
                    'flex items-center border-l-4 py-2 pl-3 pr-4 text-base font-medium',
                    isActive
                      ? 'border-blue-500 bg-blue-50 text-blue-700'
                      : 'border-transparent text-gray-500 hover:border-gray-300 hover:bg-gray-50 hover:text-gray-700'
                  )}
                >
                  <Icon
                    className={clsx(
                      'w-5 h-5 mr-3',
                      item.name === 'Imports' && getSyncIconClasses(syncStatus)
                    )}
                  />
                  {item.name}
                </Link>
              )
            })}
          </div>
        </div>
      )}
    </nav>
  )
}
