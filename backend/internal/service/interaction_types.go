package service

// InteractionType is what an interaction is, by the source of its members: the
// values of the Interactions tab's and the Threads tab's type filters.
type InteractionType string

const (
	InteractionTypeMessage InteractionType = "message"
	InteractionTypeEmail   InteractionType = "email"
	InteractionTypeEvent   InteractionType = "event"
	InteractionTypeCall    InteractionType = "call"
	InteractionTypeOther   InteractionType = "other"
)
