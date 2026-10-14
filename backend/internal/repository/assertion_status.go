package repository

// Assertion status constants: the closed vocabulary assertion.status's CHECK
// enforces (migration 067). The state machine is proposed → accepted | rejected
// and accepted → superseded | retracted; the terminal-knowledge_to CHECK enforces
// that a terminal status carries a knowledge_to. The frontend's AssertionStatus
// union is generated from this file alone (backend/tygo.yaml), so keep nothing
// else in it.
const (
	AssertionStatusProposed   = "proposed"
	AssertionStatusAccepted   = "accepted"
	AssertionStatusRejected   = "rejected"
	AssertionStatusSuperseded = "superseded"
	AssertionStatusRetracted  = "retracted"
)
