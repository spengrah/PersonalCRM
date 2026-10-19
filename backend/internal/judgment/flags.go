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

// IsCommitmentAbove reports whether the flag decision is a commitment whose
// probability exceeds the clearing floor: one that later windows re-check
// until a clearing decision answers it done.
func (f FlagDecision) IsCommitmentAbove(floor float64) bool {
	return f.Kind == FlagKindCommitment && f.Probability > floor
}

// ClearingDecision is one judgment's answer to whether an earlier commitment is
// done as of its cut-off: Done is the method's answer and Probability that
// answer's probability of done. CommitmentJudgment is the judgment holding the
// commitment's flag decision: an earlier judgment, or the clearing's own
// judgment when an earlier chunk of its night flagged the commitment. Its
// identity is the thread, the commitment's message key and the cut-off of the
// judgment that made it.
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
// window's first message, the keys of the messages new at its cut-off that it
// answered, in thread order, and the earlier commitments it re-checked. The
// zero Read is a judgment of a version before migration 099, which read its
// whole context.
//
// A night judged in more than one chunk holds each chunk's Read in Chunks,
// earliest first: the chunk's window start, the new messages it flagged and the
// earlier judgments' commitments inside its window. Its own fields are then the
// night's: the first chunk's window start, every chunk's new messages and every
// chunk's re-checks. A night judged over one window has no chunks.
type Read struct {
	WindowStart string
	NewMessages []string
	Rechecked   []Recheck
	Chunks      []Read
}

// Recheck names one earlier commitment a judgment re-checked: the earlier
// judgment holding its flag decision and the commitment's message key.
type Recheck struct {
	Judgment   uuid.UUID
	MessageKey string
}

// readOf is the Read of a judgment: its chunks, and the earlier commitments
// its clearings name.
func readOf(j Judgment) Read {
	r := Read{WindowStart: j.WindowStart, NewMessages: j.NewMessages, Chunks: j.Chunks}
	for _, c := range j.Clearings {
		r.Rechecked = append(r.Rechecked, Recheck{c.CommitmentJudgment, c.CommitmentMessageKey})
	}
	return r
}

// ballInCourtWindowStart is the first message of the window a judgment asked
// ball-in-court over: its last chunk's, since only a night's last chunk asks it.
func (r Read) ballInCourtWindowStart() string {
	if len(r.Chunks) > 0 {
		return r.Chunks[len(r.Chunks)-1].WindowStart
	}
	return r.WindowStart
}

// digest identifies the Read among a method version's judgments of one
// example: of a night judged over one window, its window start, new messages
// and re-checks; of a night judged in chunks, each chunk's, in order. It is
// empty for the zero Read, so a judgment of a version before migration 099
// keeps its identity of method version, thread and input hash.
func (r Read) digest() string {
	if r.WindowStart == "" {
		return ""
	}
	var encoded any = r.encoded()
	if len(r.Chunks) > 0 {
		chunks := make([]readEncoding, len(r.Chunks))
		for i, c := range r.Chunks {
			chunks[i] = c.encoded()
		}
		encoded = struct {
			Chunks []readEncoding `json:"chunks"`
		}{chunks}
	}
	raw, err := json.Marshal(encoded)
	if err != nil {
		panic(fmt.Sprintf("encode read: %v", err)) // strings and UUIDs always encode
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// readEncoding is one window's read as its digest encodes it.
type readEncoding struct {
	WindowStart string    `json:"window_start"`
	NewMessages []string  `json:"new_messages"`
	Rechecked   []Recheck `json:"rechecked"`
}

func (r Read) encoded() readEncoding {
	rechecked := slices.Clone(r.Rechecked)
	slices.SortFunc(rechecked, func(a, b Recheck) int {
		return cmp.Or(slices.Compare(a.Judgment[:], b.Judgment[:]), cmp.Compare(a.MessageKey, b.MessageKey))
	})
	return readEncoding{r.WindowStart, nonNil(r.NewMessages), nonNil(slices.Compact(rechecked))}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
