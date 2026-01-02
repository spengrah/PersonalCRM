'use client'

import { useState, useEffect } from 'react'

export default function TestApiPage() {
  const [status, setStatus] = useState('Testing...')
  const [result, setResult] = useState<unknown>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    async function testApi() {
      try {
        setStatus('Making fetch request...')

        const apiUrl = process.env.NEXT_PUBLIC_API_URL || ''
        const response = await fetch(`${apiUrl}/api/v1/contacts/overdue`, {
          method: 'GET',
          headers: {
            'Content-Type': 'application/json',
            'X-API-Key': process.env.NEXT_PUBLIC_API_KEY || '',
          },
        })

        setStatus(`Response received: ${response.status}`)

        if (!response.ok) {
          throw new Error(`HTTP ${response.status}: ${response.statusText}`)
        }

        const data = await response.json()
        setStatus('✅ Success')
        setResult(data)
      } catch (err: unknown) {
        setStatus('❌ Failed')
        setError(err instanceof Error ? err.message : 'Unknown error')
      }
    }

    testApi()
  }, [])

  return (
    <div className="p-8">
      <h1 className="text-2xl font-bold mb-4">Direct API Test</h1>

      <div className="space-y-4">
        <div>
          <strong>Status:</strong> {status}
        </div>

        {error && (
          <div className="text-red-600 bg-red-50 p-4 rounded">
            <strong>Error:</strong> {error}
          </div>
        )}

        {result && (
          <div className="text-green-600 bg-green-50 p-4 rounded">
            <strong>Success:</strong> {result.success ? 'true' : 'false'}
            <br />
            <strong>Data count:</strong> {result.data?.length || 0}
            {result.data && result.data.length > 0 && (
              <div>
                <br />
                <strong>First contact:</strong> {result.data[0].full_name}
                <br />
                <strong>Days overdue:</strong> {result.data[0].days_overdue}
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}
