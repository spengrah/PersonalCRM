package threadctx

// QuestionKind is the answer shape of a question.
type QuestionKind string

const (
	// QuestionNoul is a yes/no question.
	QuestionNoul QuestionKind = "noul"
	// QuestionChoice picks one of several options.
	QuestionChoice QuestionKind = "choice"
)

// Question is one question asked of a thread context. ID names its participant
// by role label, never by a contact or thread ID. ID and option keys are
// identifiers the masker passes through unchanged, so they carry no content.
type Question struct {
	ID           string
	Kind         QuestionKind
	Instructions string
	Options      []Option
}

// Option is one answer a question offers.
type Option struct {
	Key         string
	Label       string
	Description string
}
