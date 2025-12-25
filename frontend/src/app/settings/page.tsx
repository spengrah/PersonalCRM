'use client'

import { useState } from 'react'
import { Download, Upload, Settings, Database, Shield, Clock } from 'lucide-react'
import { Navigation } from '@/components/layout/navigation'
import { Button } from '@/components/ui/button'
import { useAcceleratedTime } from '@/hooks/use-accelerated-time'

export default function SettingsPage() {
  const [isExporting, setIsExporting] = useState(false)
  const [isImporting, setIsImporting] = useState(false)
  const [importFile, setImportFile] = useState<File | null>(null)
  const { environment, isAccelerated, accelerationFactor } = useAcceleratedTime()

  const handleExportData = async () => {
    setIsExporting(true)
    try {
      const response = await fetch('http://localhost:8080/api/v1/export', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
      })

      if (!response.ok) {
        throw new Error('Export failed')
      }

      const blob = await response.blob()
      const url = window.URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url

      const timestamp = new Date().toISOString().split('T')[0]
      link.download = `personal-crm-backup-${timestamp}.json`

      document.body.appendChild(link)
      link.click()
      document.body.removeChild(link)
      window.URL.revokeObjectURL(url)
    } catch (error) {
      console.error('Export error:', error)
      alert('Export failed. Please try again.')
    } finally {
      setIsExporting(false)
    }
  }

  const handleFileSelect = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    if (file) {
      setImportFile(file)
    }
  }

  const handleImportData = async () => {
    if (!importFile) return

    setIsImporting(true)
    try {
      const formData = new FormData()
      formData.append('backup', importFile)

      const response = await fetch('http://localhost:8080/api/v1/import', {
        method: 'POST',
        body: formData,
      })

      if (!response.ok) {
        throw new Error('Import failed')
      }

      const result = await response.json()
      alert(
        `Import validation successful! Found ${result.data?.metadata?.contacts_count || 0} contacts and ${result.data?.metadata?.reminders_count || 0} reminders.`
      )
    } catch (error) {
      console.error('Import error:', error)
      alert('Import failed. Please check the file format and try again.')
    } finally {
      setIsImporting(false)
    }
  }

  const formatFileSize = (bytes: number) => {
    if (bytes === 0) return '0 Bytes'
    const k = 1024
    const sizes = ['Bytes', 'KB', 'MB', 'GB']
    const i = Math.floor(Math.log(bytes) / Math.log(k))
    return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i]
  }

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />

      <div className="max-w-4xl mx-auto py-6 sm:px-6 lg:px-8">
        {/* Header */}
        <div className="mb-8">
          <div className="flex items-center space-x-3 mb-2">
            <Settings className="w-8 h-8 text-blue-600" />
            <h1 className="text-3xl font-bold text-gray-900">Settings</h1>
          </div>
          <p className="text-lg text-gray-600">Manage your Personal CRM configuration and data</p>
        </div>

        <div className="space-y-6">
          {/* Time Acceleration Status */}
          <section className="bg-white rounded-lg shadow-sm border p-6">
            <div className="flex items-center space-x-3 mb-4">
              <Clock className="w-6 h-6 text-blue-600" />
              <h2 className="text-xl font-semibold text-gray-900">Time Acceleration Status</h2>
            </div>

            <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
              <div className="p-4 bg-gray-50 rounded-lg">
                <p className="text-sm text-gray-600 mb-1">Environment</p>
                <p className="text-lg font-semibold text-gray-900 capitalize">
                  {environment || 'Production'}
                </p>
              </div>

              <div className="p-4 bg-gray-50 rounded-lg">
                <p className="text-sm text-gray-600 mb-1">Acceleration</p>
                <p className="text-lg font-semibold text-gray-900">
                  {isAccelerated ? `${accelerationFactor}x` : 'Normal (1x)'}
                </p>
              </div>

              <div className="p-4 bg-gray-50 rounded-lg">
                <p className="text-sm text-gray-600 mb-1">Status</p>
                <p
                  className={`text-lg font-semibold ${isAccelerated ? 'text-blue-600' : 'text-gray-900'}`}
                >
                  {isAccelerated ? 'Testing Mode' : 'Production Mode'}
                </p>
              </div>
            </div>

            {isAccelerated && (
              <div className="mt-4 p-3 bg-blue-50 border border-blue-200 rounded-lg">
                <p className="text-sm text-blue-800">
                  <strong>Testing Mode Active:</strong> Time is accelerated for testing purposes.
                  Birthday calculations and contact reminders are running faster than normal.
                </p>
              </div>
            )}
          </section>

          {/* Data Backup & Restore */}
          <section className="bg-white rounded-lg shadow-sm border p-6">
            <div className="flex items-center space-x-3 mb-6">
              <Database className="w-6 h-6 text-green-600" />
              <h2 className="text-xl font-semibold text-gray-900">Data Backup & Restore</h2>
            </div>

            {/* Export Section */}
            <div className="mb-8">
              <h3 className="text-lg font-medium text-gray-900 mb-3">Export Data</h3>
              <p className="text-gray-600 mb-4">
                Download a complete backup of your CRM data including contacts, reminders, notes,
                and settings.
              </p>

              <Button
                onClick={handleExportData}
                loading={isExporting}
                className="flex items-center space-x-2"
              >
                <Download className="w-4 h-4" />
                <span>{isExporting ? 'Exporting...' : 'Download Backup'}</span>
              </Button>

              <div className="mt-3 text-sm text-gray-500">
                <p>• Backup includes all contacts, reminders, interactions, and notes</p>
                <p>• File format: JSON (human-readable)</p>
                <p>• Recommended frequency: Weekly or before major changes</p>
              </div>
            </div>

            {/* Import Section */}
            <div className="border-t pt-6">
              <h3 className="text-lg font-medium text-gray-900 mb-3">Import Data</h3>
              <p className="text-gray-600 mb-4">
                Restore your CRM data from a previous backup. This will validate the backup file
                format.
              </p>

              <div className="space-y-4">
                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-2">
                    Select Backup File
                  </label>
                  <input
                    type="file"
                    accept=".json"
                    onChange={handleFileSelect}
                    className="block w-full text-sm text-gray-900 border border-gray-300 rounded-md cursor-pointer bg-gray-50 focus:outline-none focus:border-blue-500"
                  />
                </div>

                {importFile && (
                  <div className="p-3 bg-blue-50 border border-blue-200 rounded-lg">
                    <div className="flex items-center justify-between">
                      <div>
                        <p className="text-sm font-medium text-blue-900">{importFile.name}</p>
                        <p className="text-xs text-blue-700">{formatFileSize(importFile.size)}</p>
                      </div>
                      <Button
                        onClick={handleImportData}
                        loading={isImporting}
                        size="sm"
                        variant="outline"
                      >
                        <Upload className="w-4 h-4 mr-1" />
                        {isImporting ? 'Validating...' : 'Validate'}
                      </Button>
                    </div>
                  </div>
                )}
              </div>

              <div className="mt-4 p-3 bg-amber-50 border border-amber-200 rounded-lg">
                <div className="flex items-start space-x-2">
                  <Shield className="w-5 h-5 text-amber-600 mt-0.5" />
                  <div>
                    <p className="text-sm font-medium text-amber-800">Note:</p>
                    <p className="mt-1 text-sm text-amber-700">
                      Currently in validation mode. The import will validate your backup file format
                      and show a summary of the data without making changes.
                    </p>
                  </div>
                </div>
              </div>
            </div>
          </section>

          {/* System Information */}
          <section className="bg-white rounded-lg shadow-sm border p-6">
            <div className="flex items-center space-x-3 mb-4">
              <Shield className="w-6 h-6 text-purple-600" />
              <h2 className="text-xl font-semibold text-gray-900">System Information</h2>
            </div>

            <div className="grid grid-cols-1 md:grid-cols-2 gap-4 text-sm">
              <div>
                <p className="text-gray-600">Version</p>
                <p className="font-medium text-gray-900">Personal CRM v1.0</p>
              </div>

              <div>
                <p className="text-gray-600">Last Updated</p>
                <p className="font-medium text-gray-900">{new Date().toLocaleDateString()}</p>
              </div>

              <div>
                <p className="text-gray-600">Environment</p>
                <p className="font-medium text-gray-900">
                  {environment === 'testing' ? 'Development' : 'Production'}
                </p>
              </div>

              <div>
                <p className="text-gray-600">Features</p>
                <p className="font-medium text-gray-900">
                  Contacts • Reminders • Birthdays • Time Acceleration
                </p>
              </div>
            </div>
          </section>
        </div>
      </div>
    </div>
  )
}
