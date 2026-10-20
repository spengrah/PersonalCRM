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

// This file holds the message-flag records of migration 099. The enumerations
// they carry (FlagKind, UnjudgedReason) sit in values.go, which the generated
// API types mirror.

// FlagKinds lists the flag kinds, in the order a message's flag decisions read.
var FlagKinds = []FlagKind{FlagKindCommitment, FlagKindBigMoment, FlagKindClaim}

// UnjudgedReasons lists the reasons a new message is unjudged.
var UnjudgedReasons = []UnjudgedReason{UnjudgedReasonCallFailed, UnjudgedReasonMessageOverLimit, UnjudgedReasonBeforeWindow}

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

// Shows reports whether a flag decision of the probability shows under the
// display threshold: the one rule by which the Threads tab draws a chip and the
// scorecard counts a decision flagged.
func Shows(probability, threshold float64) bool {
	return probability >= threshold
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
// earlier judgments' commitments it re-checked. Its own fields are then the
// night's: the first chunk's window start and every chunk's new messages. A
// later chunk re-checks what the chunks before it left open, so its window
// start and re-checks follow from their answers, by what Chunking identifies.
// A night judged over one window has no chunks and no Chunking.
type Read struct {
	WindowStart string
	NewMessages []string
	Rechecked   []Recheck
	Chunks      []Read
	// Chunking identifies what decides a chunked night's later windows besides
	// the answers of the chunks before them: the method's sizing of a window,
	// its clearing floor and the earlier commitments the night may re-check.
	Chunking string
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
	r := Read{WindowStart: j.WindowStart, NewMessages: j.NewMessages, Chunks: j.Chunks, Chunking: j.Chunking}
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
// and re-checks; of a night judged in chunks, its window start, each chunk's
// new messages, in order, and its Chunking, which with the answers of the
// chunks before it decide every chunk's window and re-checks, so a judgment
// found by its digest read what a judgment of its plan would. It is empty for
// the zero Read, so a judgment of a version before migration 099 keeps its
// identity of method version, thread and input hash.
func (r Read) digest() string {
	if r.WindowStart == "" {
		return ""
	}
	var encoded any = r.encoded()
	if len(r.Chunks) > 0 {
		chunks := make([][]string, len(r.Chunks))
		for i, c := range r.Chunks {
			chunks[i] = nonNil(c.NewMessages)
		}
		encoded = struct {
			WindowStart string     `json:"window_start"`
			Chunks      [][]string `json:"chunks"`
			Chunking    string     `json:"chunking"`
		}{r.WindowStart, chunks, r.Chunking}
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
