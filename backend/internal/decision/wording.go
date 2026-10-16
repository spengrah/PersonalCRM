package decision

import "encoding/json"

// Wording is the text of every question in the tree. In it, {party} is a
// participant's role label; {debtor} and {creditor} are the roles of who made
// a commitment and to whom; {message}, {expression} and {date} fill a
// candidate option's label.
type Wording struct {
	// BallInCourt options are keyed by a ball-in-court outcome value, or, in a
	// reasoned variant, by "<outcome>.<reason>"; options of one outcome sum.
	BallInCourt QuestionWording
	// CommitmentMine and CommitmentTheirs are yes/no questions: the first
	// option is the yes answer (open), the second the no answer (none).
	CommitmentMine   QuestionWording
	CommitmentTheirs QuestionWording
	// CommitmentDate offers each date the thread's timing expressions resolve
	// to, labeled by DateOption, and then its own options.
	CommitmentDate QuestionWording
	DateOption     string
	// SourceMessage offers each message the commitment could be stated in,
	// labeled by MessageOption, and then its own options.
	SourceMessage QuestionWording
	MessageOption string
	BigMoment     QuestionWording
}

// QuestionWording is one question's instructions and fixed options.
type QuestionWording struct {
	Instructions string
	Options      []OptionWording
}

// OptionWording is one option: its key, the label the model reads and what
// it means.
type OptionWording struct {
	Key         string
	Label       string
	Description string
}

// clone copies the wording through the encoding its version digests, so the
// copy shares no options with it, whatever fields the wording gains.
func (w Wording) clone() Wording {
	encoded, err := json.Marshal(w)
	if err != nil {
		panic(err) // plain structs of strings always encode
	}
	var out Wording
	if err := json.Unmarshal(encoded, &out); err != nil {
		panic(err)
	}
	return out
}

// Option keys the tree reads besides outcome values. A candidate question
// always offers its own two options, so it is a choice even when the thread
// gives it no candidate; neither names a date or a message.
const (
	optionDateUnknown = "unknown"
	optionNoMessage   = "none"
	optionUnlisted    = "unlisted"
)

// DefaultWording is the plain question wording.
var DefaultWording = Wording{
	BallInCourt: QuestionWording{
		Instructions: "As of the last message, who owes the next reply between ME and {party} in this thread?",
		Options: []OptionWording{
			{Key: "i_owe_reply", Label: "ME owes {party} a reply", Description: "{party} asked ME something, asked ME to do or decide something, or said something that calls for ME's answer, and ME has not answered it."},
			{Key: "they_owe_reply", Label: "{party} owes ME a reply", Description: "ME asked {party} something, asked {party} to do or decide something, or said something that calls for {party}'s answer, and {party} has not answered it."},
			{Key: "nobody_owes", Label: "Nobody owes a reply", Description: "The exchange between ME and {party} is settled: the last message needs no answer, such as a thanks, a closer or an acknowledgement."},
			{Key: "not_applicable", Label: "Not a conversation", Description: "The thread is an automated message, a broadcast or a newsletter, not a conversation between ME and {party}."},
		},
	},
	CommitmentMine: QuestionWording{
		Instructions: "Has ME said ME would do something for {party} that ME has not yet done?",
		Options: []OptionWording{
			{Key: "open", Description: "ME promised or agreed to do something for {party}, and the thread does not show it done."},
			{Key: "none", Description: "ME made no such promise, or it is done."},
		},
	},
	CommitmentTheirs: QuestionWording{
		Instructions: "Has {party} said {party} would do something for ME that {party} has not yet done?",
		Options: []OptionWording{
			{Key: "open", Description: "{party} promised or agreed to do something for ME, and the thread does not show it done."},
			{Key: "none", Description: "{party} made no such promise, or it is done."},
		},
	},
	CommitmentDate: QuestionWording{
		Instructions: "If {debtor} said {debtor} would do something for {creditor}, by when did {debtor} say it would be done?",
		Options: []OptionWording{
			{Key: optionDateUnknown, Label: "No date stated", Description: "The thread states no time for it, or there is no such commitment."},
			{Key: optionUnlisted, Label: "A time not listed here", Description: "The thread states a time for it that none of the other options gives."},
		},
	},
	DateOption: "\"{expression}\" in {message}, meaning {date}",
	SourceMessage: QuestionWording{
		Instructions: "If {debtor} said {debtor} would do something for {creditor}, which message states that commitment?",
		Options: []OptionWording{
			{Key: optionNoMessage, Label: "No message", Description: "There is no such commitment."},
			{Key: optionUnlisted, Label: "A message not listed here", Description: "The commitment is stated in a message none of the other options names."},
		},
	},
	MessageOption: "message {message}",
	BigMoment: QuestionWording{
		Instructions: "Does the thread reveal a significant moment in someone's life, such as a new job, an illness, a birth, a death, a wedding or a move?",
		Options: []OptionWording{
			{Key: "none", Label: "No significant moment", Description: "Nothing in the thread is a significant life event."},
			{Key: "participant_moment", Label: "A participant's own moment", Description: "A significant moment in the life of someone writing or written to in the thread."},
			{Key: "other_person_moment", Label: "Someone else's moment, mentioned in passing", Description: "A significant moment in the life of a person outside the thread, such as a participant's relative or friend."},
		},
	},
}

// ReasonedWording is DefaultWording with reasons written into the
// ball-in-court labels, one option per reason.
var ReasonedWording = func() Wording {
	w := DefaultWording.clone()
	w.BallInCourt = QuestionWording{
		Instructions: DefaultWording.BallInCourt.Instructions,
		Options: []OptionWording{
			{Key: "i_owe_reply.question", Label: "ME owes {party} a reply, because {party} asked ME a question ME has not answered"},
			{Key: "i_owe_reply.request", Label: "ME owes {party} a reply, because {party} asked ME to do or decide something and ME has not responded"},
			{Key: "i_owe_reply.news", Label: "ME owes {party} a reply, because {party} shared something that calls for ME's response"},
			{Key: "they_owe_reply.question", Label: "{party} owes ME a reply, because ME asked {party} a question {party} has not answered"},
			{Key: "they_owe_reply.request", Label: "{party} owes ME a reply, because ME asked {party} to do or decide something and {party} has not responded"},
			{Key: "nobody_owes.closed", Label: "Nobody owes a reply, because the last message closes the exchange"},
			{Key: "nobody_owes.answered", Label: "Nobody owes a reply, because every question and request has been answered"},
			{Key: "not_applicable.automated", Label: "Not a conversation, because the thread is automated, a broadcast or a newsletter"},
		},
	}
	return w
}()
