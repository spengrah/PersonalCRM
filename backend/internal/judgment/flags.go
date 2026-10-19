package judgment

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// This file holds the message-flag records of migration 099. Its values sit
// outside values.go, whose constants the generated API types mirror: no
// response carries them yet.

// FlagKind is what a flag decision flags a message as holding. A flag carries
// no category and no extracted content.
type FlagKind string

const (
	// FlagKindCommitment: the message states that someone will do something for
	// another participant, and the content up to the cut-off does not show it
	// done.
	FlagKindCommitment FlagKind = "commitment"
	// FlagKindBigMoment: the message reveals a significant moment in a contact's
	// life.
	FlagKindBigMoment FlagKind = "big_moment"
	// FlagKindClaim: the message states a fact about a contact or someone in a
	// contact's life.
	FlagKindClaim FlagKind = "claim"
)

// FlagKinds lists the flag kinds, in the order a message's flag decisions read.
var FlagKinds = []FlagKind{FlagKindCommitment, FlagKindBigMoment, FlagKindClaim}

// UnjudgedReason is why a new message was answered by no judgment of a run.
type UnjudgedReason string

const (
	// UnjudgedReasonCallFailed: the cut-off's call failed after its retries.
	UnjudgedReasonCallFailed UnjudgedReason = "call_failed"
	// UnjudgedReasonMessageOverLimit: the cut-off was reported incomplete with
	// IncompleteReasonMessageOverLimit.
	UnjudgedReasonMessageOverLimit UnjudgedReason = "message_over_limit"
	// UnjudgedReasonBeforeWindow: the message lay before the window's start.
	UnjudgedReasonBeforeWindow UnjudgedReason = "before_window"
)

// IncompleteReasonMessageOverLimit is the reason for a context whose newest
// message alone exceeds the method's limits.
const IncompleteReasonMessageOverLimit IncompleteReason = "message_over_limit"

// ErrNoFlagSettings is returned for a method key with no display threshold or
// clearing floor.
var ErrNoFlagSettings = errors.New("no flag thresholds for method")

// FlagDecision is one judgment's answer for one new message and one flag kind:
// the probability that the message holds that kind, stored whatever the
// display threshold. Its identity is the thread, MessageKey and Kind, whichever
// method, run or cut-off judged the message.
type FlagDecision struct {
	MessageKey  string
	Kind        FlagKind
	Probability float64
}

// ClearingDecision is one judgment's answer to whether an earlier commitment is
// done as of its cut-off: Done is the method's answer and Probability that
// answer's probability of done. CommitmentJudgment is the earlier judgment
// holding the commitment's flag decision. Its identity is the thread, the
// commitment's message key and the cut-off of the judgment that made it.
type ClearingDecision struct {
	CommitmentMessageKey string
	CommitmentJudgment   uuid.UUID
	Done                 bool
	Probability          float64
}

// UnjudgedMessage is a new message no judgment of a run answered at a cut-off.
type UnjudgedMessage struct {
	Thread     uuid.UUID
	CutOff     time.Time
	MessageKey string
	Reason     UnjudgedReason
}

// Read is what a judgment read beside its context: the message key of its
// window's first message, the keys of the messages new at its cut-off in thread
// order, and the earlier commitments it re-checked. The zero
// Read is a judgment of a version before migration 099, which read its whole
// context.
type Read struct {
	WindowStart string
	NewMessages []string
	Rechecked   []Recheck
}

// Recheck names one earlier commitment a judgment re-checked: the earlier
// judgment holding its flag decision and the commitment's message key.
type Recheck struct {
	Judgment   uuid.UUID
	MessageKey string
}

// readOf is the Read of a judgment: the earlier commitments its clearings name.
func readOf(windowStart string, newMessages []string, clearings []ClearingDecision) Read {
	r := Read{WindowStart: windowStart, NewMessages: newMessages}
	for _, c := range clearings {
		r.Rechecked = append(r.Rechecked, Recheck{c.CommitmentJudgment, c.CommitmentMessageKey})
	}
	return r
}

// digest identifies the Read among a method version's judgments of one example;
// empty for the zero Read, so a judgment of a version before migration 099
// keeps its identity of method version, thread and input hash.
func (r Read) digest() string {
	if r.WindowStart == "" {
		return ""
	}
	rechecked := slices.Clone(r.Rechecked)
	slices.SortFunc(rechecked, func(a, b Recheck) int {
		return cmp.Or(slices.Compare(a.Judgment[:], b.Judgment[:]), cmp.Compare(a.MessageKey, b.MessageKey))
	})
	rechecked = slices.Compact(rechecked)
	raw, err := json.Marshal(struct {
		WindowStart string    `json:"window_start"`
		NewMessages []string  `json:"new_messages"`
		Rechecked   []Recheck `json:"rechecked"`
	}{r.WindowStart, nonNil(r.NewMessages), nonNil(rechecked)})
	if err != nil {
		panic(fmt.Sprintf("encode read: %v", err)) // strings and UUIDs always encode
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
