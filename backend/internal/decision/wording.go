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

// DefaultWording is the plain question wording. Its flag definitions and its
// ball-in-court rule are the user's rulings, recorded as decisions
// 20261021T163917Z-77e06cd925bb (commitment), 20261021T192457Z-c37d51338ae3
// (big moment and claim) and 20261023T012121Z-d58daa3d22c3 (ball-in-court).
var DefaultWording = Wording{
	BallInCourt: QuestionWording{
		Instructions: "As of the last message, who owes the next reply between ME and {party} in this thread? Judge by the latest message between ME and {party}: whoever sent it never owes the next reply.",
		Options: []OptionWording{
			{Key: "i_owe_reply", Label: "ME owes {party} a reply", Description: "{party}'s latest message comes after ME's latest and asks ME something, asks ME to do or decide something, invites ME, or shares something with ME personally, such as news, a story or a link meant for ME."},
			{Key: "they_owe_reply", Label: "{party} owes ME a reply", Description: "ME's latest message comes after {party}'s latest and asks {party} something, asks {party} to do or decide something, invites {party}, or nudges {party} to reply, as a bump such as \"would love to catch up soon\" does."},
			{Key: "nobody_owes", Label: "Nobody owes a reply", Description: "The latest message needs no answer: a thanks, an acknowledgement, a closer, a sticker or reaction, a \"never mind\", or a question put to a whole group rather than to one person."},
			{Key: "not_applicable", Label: "Not a conversation", Description: "The thread is an automated message, a broadcast or a newsletter, not a conversation between ME and {party}."},
		},
	},
	FlagCommitment: QuestionWording{
		Instructions: "Does message {message} state a commitment? A commitment is a promise or stated intent, by ME or another participant, to take a specific action involving the other after the conversation, one someone could later check was done. Accepting the other's request counts. It includes a promise to arrange or schedule a meeting; to get back with an answer; to send, share, review, read or check out a named thing; to pay, lend, return or bring something; an accepted introduction; a favor or plan on a later day; checking in; following up on a named matter; and ME's own commitment to attend a future event. It does not include same-day logistics or being on the way; proposing or confirming a meeting time; an offer the other must accept first; a wish with no action; a promise to keep someone posted with no named matter; a recurring plan such as \"let's do this monthly\"; a commitment relayed for someone else; or plans among other people that ME is not party to. Read the messages after message {message}: if one of them shows the thing done, message {message} states no commitment, because a commitment made and kept within the messages shown is not one. In meeting notes, which ME wrote, a commitment the note reports for anyone counts.",
		Options: []OptionWording{
			{Key: optionYes, Label: "A commitment", Description: "Message {message} promises or states the intent to take a specific action involving the other after the conversation, one someone could check was done, and no message shown after it shows it done."},
			{Key: optionNo, Label: "No commitment", Description: "Message {message} states no such commitment, states only something the definition leaves out, or a message shown after it shows the thing done."},
		},
	},
	FlagBigMoment: QuestionWording{
		Instructions: "Does message {message} reveal a big moment? A big moment is news about a significant event or change in the life of a contact, a participant other than ME, or of someone close to them, that gives ME a reason to reach out: to ask how it went, to support or to celebrate. Someone close is someone ME would naturally ask the contact about, such as a partner, child, parent or sibling, or someone else the contact clearly cares about. It includes a new or lost job or role; a company starting or shutting down; a move or a lost home; a serious illness, injury, surgery or recovery; a pregnancy, birth or adoption; an engagement, wedding, separation or divorce; a death; a major achievement or milestone; and a trip out of the ordinary for the person, far, long or rare, for leisure or for work, though not a night or two in a nearby city or a regular business trip: the test is whether a friend would ask how it went afterward. New information about a big moment already known counts, such as a surgery that went well or a job that started; a mention that adds nothing new, such as a congratulation or a question not yet answered, does not. A moment in ME's own life, an event in the life of someone outside the contact's close circle, and a routine or minor event are never big moments.",
		Options: []OptionWording{
			{Key: optionYes, Label: "A big moment", Description: "Message {message} reveals a significant event or change in the life of a contact, or of someone close to them, that gives ME a reason to reach out, or new information about one."},
			{Key: optionNo, Label: "No big moment", Description: "Message {message} reveals no such event, or only a moment in ME's own life, an event outside the contact's close circle, a routine or minor event, or a mention that adds nothing new."},
		},
	},
	FlagClaim: QuestionWording{
		Instructions: "Does message {message} state a claim? A claim is a lasting fact about a contact, a participant other than ME, or about someone close to them, worth remembering the next time ME talks to them. It includes family, partners, and children with their ages and interests; pets; where they live; their employer, their role and what they build; interests, tastes and preferences, gift ideas among them; health conditions and needs; important dates; how they like to be contacted; and strongly held views. A fact about ME, a passing state or plan, a fact about someone outside the contact's close circle, a job opening or company news that is not about the contact's own role, and a fact already stated earlier in the messages shown are never claims.",
		Options: []OptionWording{
			{Key: optionYes, Label: "A claim", Description: "Message {message} states a lasting fact about a contact, or someone close to them, worth remembering the next time ME talks to them."},
			{Key: optionNo, Label: "No claim", Description: "Message {message} states no such fact, or only a fact about ME, a passing state or plan, a fact outside the contact's close circle, company news not about the contact's own role, or a fact stated earlier."},
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
			{Key: "i_owe_reply.question", Label: "ME owes {party} a reply, because {party}'s latest message, after ME's, asks ME a question"},
			{Key: "i_owe_reply.request", Label: "ME owes {party} a reply, because {party}'s latest message, after ME's, asks ME to do or decide something or invites ME"},
			{Key: "i_owe_reply.news", Label: "ME owes {party} a reply, because {party}'s latest message, after ME's, shares something with ME personally"},
			{Key: "they_owe_reply.question", Label: "{party} owes ME a reply, because ME's latest message, after {party}'s, asks {party} a question"},
			{Key: "they_owe_reply.request", Label: "{party} owes ME a reply, because ME's latest message, after {party}'s, asks {party} to do or decide something or invites {party}"},
			{Key: "they_owe_reply.bump", Label: "{party} owes ME a reply, because ME's latest message, after {party}'s, nudges {party} to reply"},
			{Key: "nobody_owes.closed", Label: "Nobody owes a reply, because the latest message needs no answer: a thanks, an acknowledgement, a closer, a sticker or reaction, or a \"never mind\""},
			{Key: "nobody_owes.answered", Label: "Nobody owes a reply, because every question and request has been answered"},
			{Key: "nobody_owes.group", Label: "Nobody owes a reply, because the latest message puts its question to a whole group rather than to one person"},
			{Key: "not_applicable.automated", Label: "Not a conversation, because the thread is automated, a broadcast or a newsletter"},
		},
	}
	return w
}()
