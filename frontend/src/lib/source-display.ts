import { Users, Calendar, HelpCircle } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'

export interface SourceDisplayInfo {
  label: string
  icon: LucideIcon
}

const SOURCE_DISPLAY_MAP: Record<string, SourceDisplayInfo> = {
  gcontacts: { label: 'Google Contacts', icon: Users },
  gcal_attendee: { label: 'Google Calendar', icon: Calendar },
}

/**
 * Get friendly display information for a source identifier.
 * Returns the source label and an appropriate icon.
 */
export function getSourceDisplay(source: string): SourceDisplayInfo {
  return SOURCE_DISPLAY_MAP[source] || { label: source, icon: HelpCircle }
}
