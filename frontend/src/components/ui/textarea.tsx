import { TextareaHTMLAttributes, forwardRef } from 'react'
import { clsx } from 'clsx'

interface TextareaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  label?: string
  error?: string
  helpText?: string
}

const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(
  ({ className, label, error, helpText, id, ...props }, ref) => {
    const textareaId = id || label?.toLowerCase().replace(/\s+/g, '-')

    return (
      <div className="space-y-1">
        {label && (
          <label htmlFor={textareaId} className="block text-sm font-medium text-gray-700">
            {label}
            {props.required && <span className="text-red-500 ml-1">*</span>}
          </label>
        )}
        <textarea
          ref={ref}
          id={textareaId}
          rows={3}
          className={clsx(
            'block w-full rounded-md border border-gray-300 shadow-sm',
            'px-3 py-2 sm:text-sm',
            'text-gray-900 placeholder-gray-400',
            'transition-colors',
            'focus:border-blue-500 focus:ring-1 focus:ring-blue-500',
            'disabled:bg-gray-50 disabled:text-gray-500',
            error && 'border-red-300 focus:border-red-500 focus:ring-red-500',
            className
          )}
          {...props}
        />
        {error && <p className="text-sm text-red-600">{error}</p>}
        {helpText && !error && <p className="text-sm text-gray-500">{helpText}</p>}
      </div>
    )
  }
)

Textarea.displayName = 'Textarea'

export { Textarea }
