import { apiClient } from './api-client'

export type RematchJobStatus = 'running' | 'completed' | 'failed'

export interface RematchJobMethod {
  type: string
  value: string
}

export interface RematchJob {
  id: string
  contact_id: string
  status: RematchJobStatus
  matched: number
  methods: RematchJobMethod[]
  started_at: string
  completed_at?: string | null
  error?: string | null
}

export interface RescanResponse {
  rematch_job_id: string | null
}

export const rematchApi = {
  // Fetch the current state of a rematch job. Returns 404 (thrown) once the
  // job is no longer known (e.g. process restart cleared the in-memory map).
  getJob: (jobId: string): Promise<RematchJob> =>
    apiClient.get<RematchJob>(`/api/v1/rematch/jobs/${jobId}`),

  // Trigger a full rematch across all of a contact's current methods. Returns
  // null jobID when no method types match a registered handler.
  rescan: (contactId: string): Promise<RescanResponse> =>
    apiClient.post<RescanResponse>(`/api/v1/rematch/contacts/${contactId}/rescan`, {}),
}
