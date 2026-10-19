package decision

import "encoding/json"

// Wording is the text of every question a method asks. In it, {party} is a
// participant's role label and {message} the reference the rendered state
// gives one message (m1, m2, and so on).
type Wording struct {
	// BallInCourt options are keyed by a ball-in-court outcome value, or, in a
	// reasoned variant, by "<outcome>.<reason>"; options of one outcome sum.
	BallInCourt QuestionWording
	// FlagCommitment, FlagBigMoment and FlagClaim are yes/no questions about
	// one new message: the first option is the yes answer, the second the no
	// answer.
	FlagCommitment QuestionWording
	FlagBigMoment  QuestionWording
	FlagClaim      QuestionWording
	// Clearing is a yes/no question about the commitment one earlier message
	// states: the first option is the done answer, the second the not done
	// answer.
	Clearing QuestionWording
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

// Option keys of the yes/no questions: the first option of each is yes.
const (
	optionYes     = "yes"
	optionNo      = "no"
	optionDone    = "done"
	optionNotDone = "not_done"
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
	FlagCommitment: QuestionWording{
		Instructions: "Does message {message} state a commitment? A commitment is a statement that someone will do something for another participant, that the messages shown, up to the last one, do not show done. Read the messages after message {message}: if one of them shows the thing done, message {message} states no commitment, because a commitment made and kept within the messages shown is not one. A commitment ME makes counts. In meeting notes, which ME wrote, a commitment the note reports for anyone counts.",
		Options: []OptionWording{
			{Key: optionYes, Label: "A commitment", Description: "Message {message} states that someone will do something for another participant, and no message shown after it shows it done."},
			{Key: optionNo, Label: "No commitment", Description: "Message {message} states no such commitment, or a message shown after it shows the thing done."},
		},
	},
	FlagBigMoment: QuestionWording{
		Instructions: "Does message {message} reveal a big moment? A big moment is a significant moment in the life of a contact, a participant other than ME, or of someone in a contact's life, such as a new job, an illness, a birth, a death, a wedding or a move. A moment in ME's own life is never a big moment.",
		Options: []OptionWording{
			{Key: optionYes, Label: "A big moment", Description: "Message {message} reveals a significant moment in the life of a contact, a participant other than ME, or of someone in a contact's life."},
			{Key: optionNo, Label: "No big moment", Description: "Message {message} reveals no such moment in the life of a contact or someone in a contact's life, or only a moment in ME's own life."},
		},
	},
	FlagClaim: QuestionWording{
		Instructions: "Does message {message} state a claim? A claim is a fact about a contact, a participant other than ME, or about someone in a contact's life: a preference, biography, contact details or a relationship. A fact about ME is never a claim.",
		Options: []OptionWording{
			{Key: optionYes, Label: "A claim", Description: "Message {message} states a fact about a contact, a participant other than ME, or about someone in a contact's life."},
			{Key: optionNo, Label: "No claim", Description: "Message {message} states no such fact about a contact or someone in a contact's life, or only a fact about ME."},
		},
	},
	Clearing: QuestionWording{
		Instructions: "Message {message} states a commitment. As of the last message shown, is that commitment done?",
		Options: []OptionWording{
			{Key: optionDone, Label: "Done", Description: "The messages shown, up to the last one, show that what message {message} commits someone to do has been done."},
			{Key: optionNotDone, Label: "Not done", Description: "The messages shown, up to the last one, do not show it done."},
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
