package scorecard

import (
	"context"
	"time"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"
)

// Rate is N of Of. A rate whose denominator is zero has no value, so a reader
// never takes an absent measurement for a measured zero.
type Rate struct {
	N, Of int
}

// Value is the rate, and false when its denominator is zero.
func (r Rate) Value() (float64, bool) {
	if r.Of == 0 {
		return 0, false
	}
	return float64(r.N) / float64(r.Of), true
}

// FlagMetrics count one flag kind's labeled flag decisions of a rerun by
// whether the decision shows, its probability at or above the method's display
// threshold, and whether the user's label says the message holds the kind. A
// label a close or Confirm Nones wrote counts as any other; a decision without
// a label counts in no cell.
type FlagMetrics struct {
	ShownHeld, ShownNotHeld, UnshownHeld, UnshownNotHeld int
}

// Precision is the share of shown labeled decisions whose message holds the
// kind.
func (f FlagMetrics) Precision() Rate {
	return Rate{N: f.ShownHeld, Of: f.ShownHeld + f.ShownNotHeld}
}

// Recall is the share of labeled decisions whose message holds the kind that
// show.
func (f FlagMetrics) Recall() Rate {
	return Rate{N: f.ShownHeld, Of: f.ShownHeld + f.UnshownHeld}
}

// ClearingMetrics count a rerun's labeled clearing decisions by the method's
// answer, then the user's label. A decision without a label counts in no cell.
type ClearingMetrics struct {
	DoneLabeledDone, DoneLabeledNotDone, NotDoneLabeledDone, NotDoneLabeledNotDone int
}

// Accuracy is the share of labeled clearing decisions the method answered as
// the label says.
func (c ClearingMetrics) Accuracy() Rate {
	right := c.DoneLabeledDone + c.NotDoneLabeledNotDone
	return Rate{N: right, Of: right + c.DoneLabeledNotDone + c.NotDoneLabeledDone}
}

// MessageCounts count a rerun's new messages: those its judgments answered,
// those no judgment answered, by reason, and per flag kind those whose flag
// decision shows. An unjudged message is never counted as answered.
type MessageCounts struct {
	Judged   int
	Unjudged map[judgment.UnjudgedReason]int
	Flagged  map[judgment.FlagKind]int
}

// flagScore is a rerun's flag metrics, clearing metrics and message counts.
type flagScore struct {
	flags    map[judgment.FlagKind]FlagMetrics
	clearing ClearingMetrics
	messages MessageCounts
}

// flagKey names a flag decision of a thread.
type flagKey struct {
	thread  threadctx.VenueID
	message string
	kind    judgment.FlagKind
}

// clearingKey names a clearing decision of a thread.
type clearingKey struct {
	thread     threadctx.VenueID
	commitment string
	cutOff     time.Time
}

// scoreFlags scores the flag and clearing decisions of the judgments appearing
// in a run against the labels of their threads, and counts its new messages.
func (r *Reader) scoreFlags(ctx context.Context, run judgment.RunSummary) (flagScore, error) {
	views, err := r.store.ListJudgments(ctx, judgment.JudgmentQuery{Run: &run.ID})
	if err != nil {
		return flagScore{}, failed(Failure{Reason: FailureReadRun, Run: run.ID}, err)
	}
	flagLabels, clearingLabels := map[flagKey]bool{}, map[clearingKey]bool{}
	read := map[threadctx.VenueID]bool{}
	for _, v := range views {
		if read[v.Thread] {
			continue
		}
		read[v.Thread] = true
		labels, err := r.store.LabelsByThread(ctx, v.Thread)
		if err != nil {
			return flagScore{}, failed(Failure{Reason: FailureReadLabels, Run: run.ID}, err)
		}
		for _, l := range labels.Flags {
			flagLabels[flagKey{l.Thread, l.MessageKey, l.Kind}] = l.Holds
		}
		for _, l := range labels.Clearings {
			clearingLabels[clearingKey{l.Thread, l.CommitmentMessageKey, l.CutOff.UTC()}] = l.Done
		}
	}
	out := flagScore{
		flags:    map[judgment.FlagKind]FlagMetrics{},
		messages: MessageCounts{Unjudged: run.UnjudgedCounts, Flagged: map[judgment.FlagKind]int{}},
	}
	for _, kind := range judgment.FlagKinds {
		out.flags[kind] = FlagMetrics{}
		out.messages.Flagged[kind] = 0
	}
	// Thresholds are read only for a kind a decision has, so a run holding no
	// flag decision reads none.
	thresholds := map[judgment.FlagKind]float64{}
	for _, v := range views {
		out.messages.Judged += len(v.NewMessages)
		for _, f := range v.Flags {
			threshold, ok := thresholds[f.Kind]
			if !ok {
				if threshold, err = r.store.DisplayThreshold(ctx, run.Method.Key, f.Kind); err != nil {
					return flagScore{}, failed(Failure{Reason: FailureReadThreshold, Method: run.Method.Key, Run: run.ID}, err)
				}
				thresholds[f.Kind] = threshold
			}
			shown := judgment.Shows(f.Probability, threshold)
			if shown {
				out.messages.Flagged[f.Kind]++
			}
			holds, labeled := flagLabels[flagKey{v.Thread, f.MessageKey, f.Kind}]
			if !labeled {
				continue
			}
			m := out.flags[f.Kind]
			switch {
			case shown && holds:
				m.ShownHeld++
			case shown:
				m.ShownNotHeld++
			case holds:
				m.UnshownHeld++
			default:
				m.UnshownNotHeld++
			}
			out.flags[f.Kind] = m
		}
		for _, c := range v.Clearings {
			done, labeled := clearingLabels[clearingKey{v.Thread, c.CommitmentMessageKey, v.CutOff.UTC()}]
			if !labeled {
				continue
			}
			switch {
			case c.Done && done:
				out.clearing.DoneLabeledDone++
			case c.Done:
				out.clearing.DoneLabeledNotDone++
			case done:
				out.clearing.NotDoneLabeledDone++
			default:
				out.clearing.NotDoneLabeledNotDone++
			}
		}
	}
	return out, nil
}
