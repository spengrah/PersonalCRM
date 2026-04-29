// Contact Task types — post-046 kind/lifecycle taxonomy.
//
// kind:
//   reach_out — outbound message/call; spawns follow-up
//   send      — outbound one-shot send (e.g., gift); does not spawn follow-up
//   reminder  — local-only reminder; no event, no interaction
//   meet      — legacy mutual interaction; no new instances created
//   action    — legacy ad-hoc task; no new instances created
//
// lifecycle:
//   manual         — user picker / legacy action / legacy meet
//   cadence_due    — scheduler/Todoist provider creates from contact_by
//   followup_loop  — FollowUpManager creates after outbound interaction

export type ContactTaskKind = 'reach_out' | 'send' | 'reminder' | 'meet' | 'action'
export type ContactTaskLifecycle = 'manual' | 'cadence_due' | 'followup_loop'
export type ContactTaskState =
  | 'managed'
  | 'completed'
  | 'unmanaged'
  | 'dismissed'
  | 'pending_remote_create'

// User-pickable kinds for the AddTaskModal segmented control.
export type ManualTaskKind = 'reach_out' | 'send' | 'reminder'

export interface ContactTask {
  id: string
  contact_id: string
  kind: ContactTaskKind
  lifecycle: ContactTaskLifecycle
  external_task_id: string
  content?: string
  due_date?: string
  project_id?: string
  state: ContactTaskState
  created_at: string
}

export interface CreateManualTaskRequest {
  kind: ManualTaskKind
  text: string
  notes?: string
}

export interface ContactTaskListParams {
  state?: ContactTaskState
  kind?: ContactTaskKind
  lifecycle?: ContactTaskLifecycle
}
