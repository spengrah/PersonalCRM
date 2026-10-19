package factory

// FlagSpec is a synthetic flag decision: one message, one flag kind and the
// probability that the message holds it. It carries no category and no content.
type FlagSpec struct {
	MessageKey  string
	Kind        string
	Probability float64
}

// ClearingSpec is a synthetic clearing decision on an earlier commitment: the
// method's answer and that answer's probability of done. The caller names the
// earlier judgment holding the commitment's flag decision when it records the
// spec, since the factory knows no judgment.
type ClearingSpec struct {
	CommitmentMessageKey string
	Done                 bool
	Probability          float64
}

// UnjudgedSpec is a synthetic new message no judgment answered, and why.
type UnjudgedSpec struct {
	MessageKey string
	Reason     string
}

// MessageFlagsSpec is what a synthetic judgment read of a thread and answered
// about its new messages: the window's first message, the messages new at the
// cut-off and a flag decision per new message and kind.
type MessageFlagsSpec struct {
	WindowStart string
	NewMessages []string
	Flags       []FlagSpec
}

// The builders below name messages only by the keys the caller passes and
// kinds and reasons only by the strings the caller passes (the judgment store
// owns both lists), so a spec holds no person and no text. Each probability is
// a whole percent, so it stores and reads back exactly.

// MessageFlags builds a judgment that reads the thread from a drawn message on:
// every message from there is new, and each is flagged once per kind.
func (g *Generator) MessageFlags(messageKeys, kinds []string) MessageFlagsSpec {
	if len(messageKeys) == 0 {
		return MessageFlagsSpec{}
	}
	start := g.rng.IntN(len(messageKeys))
	spec := MessageFlagsSpec{WindowStart: messageKeys[start], NewMessages: append([]string(nil), messageKeys[start:]...)}
	for _, key := range spec.NewMessages {
		for _, kind := range kinds {
			spec.Flags = append(spec.Flags, FlagSpec{MessageKey: key, Kind: kind, Probability: g.percent()})
		}
	}
	return spec
}

// Clearings builds one clearing decision per commitment message key.
func (g *Generator) Clearings(commitmentKeys []string) []ClearingSpec {
	clearings := make([]ClearingSpec, len(commitmentKeys))
	for i, key := range commitmentKeys {
		clearings[i] = ClearingSpec{CommitmentMessageKey: key, Done: g.rng.IntN(2) == 0, Probability: g.percent()}
	}
	return clearings
}

// UnjudgedMessages builds one unjudged message per message key, each with a
// drawn reason.
func (g *Generator) UnjudgedMessages(messageKeys, reasons []string) []UnjudgedSpec {
	unjudged := make([]UnjudgedSpec, len(messageKeys))
	for i, key := range messageKeys {
		unjudged[i] = UnjudgedSpec{MessageKey: key, Reason: reasons[g.rng.IntN(len(reasons))]}
	}
	return unjudged
}

// percent draws a probability of 0.00 to 1.00 in whole percents.
func (g *Generator) percent() float64 { return float64(g.rng.IntN(101)) / 100 }
