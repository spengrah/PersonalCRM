import { Users, Calendar, Send, Cloud, MessageCircle, HelpCircle } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'

export interface SourceDisplayInfo {
  label: string
  icon: LucideIcon
}

const SOURCE_DISPLAY_MAP: Record<string, SourceDisplayInfo> = {
  gcontacts: { label: 'Google Contacts', icon: Users },
  gcal_attendee: { label: 'Google Calendar', icon: Calendar },
  icloud_contacts: { label: 'iCloud Contacts', icon: Cloud },
  telegram: { label: 'Telegram', icon: Send },
  // anarlog_humans candidates flow through the ordinary candidate queue;
  // anarlog_title discovery rows live behind the dedicated People-tab
  // discovery section and never carry this badge.
  anarlog_humans: { label: 'Anarlog', icon: MessageCircle },
}

/**
 * Get friendly display information for a source identifier.
 * Returns the source label and an appropriate icon.
 */
export function getSourceDisplay(source: string): SourceDisplayInfo {
  return SOURCE_DISPLAY_MAP[source] || { label: source, icon: HelpCircle }
}
