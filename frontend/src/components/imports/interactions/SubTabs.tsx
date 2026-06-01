'use client'

export type ImportsTab = 'people' | 'interactions'

interface SubTabsProps {
  active: ImportsTab
  /** Amber badge count on the Interactions tab (conflicts + orphans). */
  attentionCount: number
  onChange: (tab: ImportsTab) => void
}

/**
 * Underline sub-tab bar for the Imports page: People / Interactions.
 * Driven by the `?tab` URL param (the parent owns the URL). The
 * Interactions tab carries an amber count badge of conflicts + orphans
 * (name candidates are NOT counted).
 */
export function SubTabs({ active, attentionCount, onChange }: SubTabsProps) {
  return (
    <div className="mb-6 border-b border-gray-200" role="tablist" aria-label="Imports sections">
      <nav className="-mb-px flex gap-6">
        <TabButton
          label="People"
          isActive={active === 'people'}
          onClick={() => onChange('people')}
        />
        <TabButton
          label="Interactions"
          isActive={active === 'interactions'}
          onClick={() => onChange('interactions')}
          badge={attentionCount}
        />
      </nav>
    </div>
  )
}

function TabButton({
  label,
  isActive,
  onClick,
  badge,
}: {
  label: string
  isActive: boolean
  onClick: () => void
  badge?: number
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={isActive}
      onClick={onClick}
      className={`relative inline-flex items-center gap-2 whitespace-nowrap border-b-2 px-1 py-3 text-sm font-medium transition-colors ${
        isActive
          ? 'border-blue-600 text-blue-700'
          : 'border-transparent text-gray-500 hover:border-gray-300 hover:text-gray-700'
      }`}
    >
      {label}
      {badge !== undefined && badge > 0 && (
        <span
          className="inline-flex min-w-[1.25rem] items-center justify-center rounded-full bg-amber-600 px-1.5 py-0.5 text-xs font-semibold text-white"
          aria-label={`${badge} needing attention`}
        >
          {badge}
        </span>
      )}
    </button>
  )
}
