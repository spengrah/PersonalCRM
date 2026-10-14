import type {
  InteractionListItemResponse,
  TimelineIdentityResponse,
} from '@/types/generated/contact'

/** One item of the contact interaction list, as the API gives it. */
export type TimelineItem = InteractionListItemResponse

/** The typed identity of an item; its `kind` decides which actions it offers. */
export type TimelineIdentity = TimelineIdentityResponse

/** Whose participation the item is read in: direction and attendee-scoped actions are this contact's. */
export type ParticipationContext = { contactId: string }

export type TimelineRowAction = {
  /** Unique within one row. */
  key: string
  label: string
  onSelect: (identity: TimelineIdentity, context: ParticipationContext) => void
}

export type TimelineRowSelection = {
  selected: boolean
  onToggle: (identity: TimelineIdentity) => void
}
