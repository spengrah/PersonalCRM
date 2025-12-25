'use client'

import { Component, ErrorInfo, ReactNode } from 'react'
import { AlertCircle, RefreshCw } from 'lucide-react'
import { Button } from '@/components/ui/button'

interface ErrorBoundaryProps {
  children: ReactNode
}

interface ErrorBoundaryState {
  hasError: boolean
  error: Error | null
  errorInfo: ErrorInfo | null
}

export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  constructor(props: ErrorBoundaryProps) {
    super(props)
    this.state = {
      hasError: false,
      error: null,
      errorInfo: null,
    }
  }

  static getDerivedStateFromError(error: Error): Partial<ErrorBoundaryState> {
    return { hasError: true, error }
  }

  componentDidCatch(error: Error, errorInfo: ErrorInfo): void {
    if (process.env.NODE_ENV === 'development') {
      console.error('ErrorBoundary caught an error:', error, errorInfo)
    }

    this.setState({
      error,
      errorInfo,
    })
  }

  handleReload = (): void => {
    window.location.reload()
  }

  render(): ReactNode {
    if (this.state.hasError) {
      return (
        <div className="min-h-screen bg-gray-50 flex items-center justify-center p-4">
          <div className="max-w-2xl w-full bg-white rounded-lg shadow-lg border border-gray-200 p-8">
            <div className="flex items-start space-x-4">
              <div className="flex-shrink-0">
                <div className="w-12 h-12 bg-red-100 rounded-full flex items-center justify-center">
                  <AlertCircle className="w-6 h-6 text-red-600" />
                </div>
              </div>

              <div className="flex-1">
                <h1 className="text-2xl font-bold text-gray-900 mb-2">Something went wrong</h1>
                <p className="text-gray-600 mb-6">
                  We apologize for the inconvenience. The application encountered an unexpected
                  error. Please try reloading the page.
                </p>

                <div className="mb-6">
                  <Button onClick={this.handleReload} variant="primary">
                    <RefreshCw className="w-4 h-4 mr-2" />
                    Reload Page
                  </Button>
                </div>

                {process.env.NODE_ENV === 'development' && this.state.error && (
                  <details className="mt-6">
                    <summary className="text-sm font-medium text-gray-700 cursor-pointer hover:text-gray-900 mb-2">
                      Error Details (Development Only)
                    </summary>
                    <div className="bg-red-50 border border-red-200 rounded-md p-4 mt-2">
                      <div className="mb-3">
                        <p className="text-sm font-semibold text-red-800 mb-1">Error Message:</p>
                        <p className="text-sm text-red-700 font-mono">{this.state.error.message}</p>
                      </div>

                      {this.state.error.stack && (
                        <div>
                          <p className="text-sm font-semibold text-red-800 mb-1">Stack Trace:</p>
                          <pre className="text-xs text-red-700 overflow-auto max-h-64 bg-red-100 p-2 rounded">
                            {this.state.error.stack}
                          </pre>
                        </div>
                      )}

                      {this.state.errorInfo?.componentStack && (
                        <div className="mt-3">
                          <p className="text-sm font-semibold text-red-800 mb-1">
                            Component Stack:
                          </p>
                          <pre className="text-xs text-red-700 overflow-auto max-h-64 bg-red-100 p-2 rounded">
                            {this.state.errorInfo.componentStack}
                          </pre>
                        </div>
                      )}
                    </div>
                  </details>
                )}
              </div>
            </div>
          </div>
        </div>
      )
    }

    return this.props.children
  }
}
