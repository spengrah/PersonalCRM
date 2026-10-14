export const SOURCE_LABELS: Record<string, string> = {
  manual: 'Manual',
  gcal: 'Calendar',
  todoist: 'Todoist',
  telegram: 'Telegram',
  messages: 'iMessage',
  anarlog_sessions: 'Meeting',
  phone_calls: 'Call',
  email: 'Email',
  gchat: 'Google Chat',
  whatsapp: 'WhatsApp',
}

export const CHANNEL_LABELS: Record<string, string> = {
  email_thread: 'Email thread',
  group_chat: 'Group chat',
  dm: 'Direct message',
  meeting: 'Meeting',
  session: 'Session',
}

export function formatDateTime(iso: string): string {
  return new Intl.DateTimeFormat('en-US', {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  }).format(new Date(iso))
}

export function formatTime(iso: string): string {
  return new Intl.DateTimeFormat('en-US', { hour: 'numeric', minute: '2-digit' }).format(
    new Date(iso)
  )
}
