package service

// InteractionType is what a row of the all-interactions view is, by the source
// of its members: the values of the view's type filter.
type InteractionType string

const (
	InteractionTypeMessage  InteractionType = "message"
	InteractionTypeEmail    InteractionType = "email"
	InteractionTypeCalendar InteractionType = "calendar"
	InteractionTypeCall     InteractionType = "call"
	InteractionTypeOther    InteractionType = "other"
)
