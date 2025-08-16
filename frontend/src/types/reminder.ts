export interface Reminder {
  id: string
  contact_id?: string
  title: string
  description?: string
  due_date: string
  completed: boolean
  completed_at?: string
  created_at: string
  deleted_at?: string
}

export interface DueReminder extends Reminder {
  contact_name?: string
  contact_email?: string
}

export interface CreateReminderRequest {
  contact_id?: string
  title: string
  description?: string
  due_date: string
}

export interface UpdateReminderRequest {
  title?: string
  description?: string
  due_date?: string
}

export interface ReminderListParams {
  page?: number
  limit?: number
  due_today?: boolean
}

export interface ReminderStats {
  total_reminders: number
  due_today: number
  overdue: number
}

