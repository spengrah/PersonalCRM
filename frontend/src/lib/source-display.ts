import { Users, Calendar, Send, Cloud, MessageCircle, Mail, HelpCircle } from 'lucide-react'
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
  // lucide-react ships no WhatsApp glyph; MessageCircle is the generic
  // chat mark already used for anarlog_humans.
  whatsapp: { label: 'WhatsApp', icon: MessageCircle },
  // anarlog_humans candidates flow through the ordinary candidate queue;
  // anarlog_title name-candidate rows live behind the dedicated People-tab
  // name-candidates section and never carry this badge.
  anarlog_humans: { label: 'Anarlog', icon: MessageCircle },
  // gmail_correspondence is a link-only source (the link-only candidate and
  // the method-suggestion card both render this label).
  gmail_correspondence: { label: 'Email correspondence', icon: Mail },
  // gmail_participant candidates are discovered from mail headers, not from
  // an established correspondence pattern, so it gets a distinct label
  // while sharing the mail icon family.
  gmail_participant: { label: 'Gmail participant', icon: Mail },
}

/**
 * Get friendly display information for a source identifier.
 * Returns the source label and an appropriate icon.
 */
export function getSourceDisplay(source: string): SourceDisplayInfo {
  return SOURCE_DISPLAY_MAP[source] || { label: source, icon: HelpCircle }
}
