import { QueryClient } from '@tanstack/react-query'

// In E2E tests, disable caching so tests always get fresh data after seeding
// Playwright sets window.__PLAYWRIGHT__ = true via addInitScript
const isPlaywright = typeof window !== 'undefined' && '__PLAYWRIGHT__' in window

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: isPlaywright ? 0 : 1000 * 60 * 5, // 0 in E2E tests, 5 minutes in prod
      gcTime: 1000 * 60 * 10, // 10 minutes
      retry: (failureCount, error) => {
        // Don't retry on 4xx errors except 408, 429
        if (error instanceof Error && 'status' in error) {
          const status = (error as { status: number }).status
          if (status >= 400 && status < 500 && ![408, 429].includes(status)) {
            return false
          }
        }
        return failureCount < 3
      },
    },
    mutations: {
      retry: (failureCount, error) => {
        // Don't retry mutations on 4xx errors
        if (error instanceof Error && 'status' in error) {
          const status = (error as { status: number }).status
          if (status >= 400 && status < 500) {
            return false
          }
        }
        return failureCount < 2
      },
    },
  },
})
