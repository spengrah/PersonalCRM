// Contact Task types for action tasks and cadence tasks

export interface ContactTask {
  id: string
  contact_id: string
  kind: 'action' | 'cadence' | 'follow_up'
  external_task_id: string
  content?: string
  due_date?: string
  project_id?: string
  state: 'managed' | 'completed' | 'unmanaged' | 'dismissed'
  created_at: string
}

export interface CreateActionTaskRequest {
  text: string
  notes?: string
}

export interface ContactTaskListParams {
  state?: 'managed' | 'completed' | 'unmanaged' | 'dismissed'
  kind?: 'action' | 'cadence' | 'follow_up'
}
