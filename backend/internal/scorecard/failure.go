package scorecard

import (
	"errors"
	"strings"

	"personal-crm/backend/internal/judgment"

	"github.com/google/uuid"
)

// FailureReason says what the scorecard failed to do.
type FailureReason string

const (
	FailureUnknownMethod      FailureReason = "unknown decision method"
	FailureMethodUnavailable  FailureReason = "decision method route not configured"
	FailureCompareNeedsMethod FailureReason = "a compare method needs a method to compare with"
	FailureReadReviews        FailureReason = "read reviewed examples"
	FailureRerun              FailureReason = "rerun method over reviewed examples"
	FailureRunNotFound        FailureReason = "run not found"
	FailureRunNotRerun        FailureReason = "run is not a rerun"
	FailureRunNotFinished     FailureReason = "run has not finished"
	FailureReadRun            FailureReason = "read run"
	FailureReadLabels         FailureReason = "read flag and clearing labels"
	FailureReadThreshold      FailureReason = "read display threshold"
	FailureReference          FailureReason = "evaluate timestamp rule"
	// FailureUnexplained is a failure no step named; only failureOf makes it.
	FailureUnexplained FailureReason = "unexplained failure"
)

// Failure is every error the scorecard returns: what failed, and the method
// key, run ID and example ID it failed for where known. It holds no cause,
// since a cause from the store, the runner or a method can name a thread or a
// contact, so no such identifier reaches output through a failure.
type Failure struct {
	Reason  FailureReason
	Method  string
	Run     judgment.RunID
	Example judgment.ExampleID
}

func (f *Failure) Error() string {
	var b strings.Builder
	b.WriteString(string(f.Reason))
	if f.Method != "" {
		b.WriteString(" method=" + f.Method)
	}
	if f.Run != uuid.Nil {
		b.WriteString(" run=" + f.Run.String())
	}
	if f.Example != "" {
		b.WriteString(" example=" + string(f.Example))
	}
	return b.String()
}

// failed is the Failure for a step that failed with a cause. It drops the
// cause on purpose: the cause's text can name a thread or a contact.
func failed(f Failure, _ error) *Failure {
	return &f
}

// failureOf is the one translation of an error into what the scorecard's
// exported functions return: the Failure it carries, without the text that
// wraps it, or an unexplained Failure.
func failureOf(err error) error {
	if err == nil {
		return nil
	}
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return &Failure{Reason: FailureUnexplained}
}
