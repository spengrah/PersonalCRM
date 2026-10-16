package judgment

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// defaultNudgeDays times a nudge for a contact without a cadence.
const defaultNudgeDays = 7

// dateFormat is a task due date's form, as task metadata stores it.
const dateFormat = "2006-01-02"

// landTx lands a live appearance of a judgment. Each outcome not yet landed,
// and not on a question the user already reviewed for this input, is planned
// against live state, asserted under the class its plan has (addition,
// removal or no effect), and applied when the catalog lands it accepted. An
// open obligation the judgment does not mention raises an omission. An outcome
// that landed before is never landed again.
func (s *Store) landTx(ctx context.Context, tx pgx.Tx, judgmentID uuid.UUID, thread uuid.UUID, cutOff time.Time) (Effects, error) {
	q := db.New(tx)
	rows, err := q.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{judgmentID}, ExampleIds: []string{}})
	if err != nil || len(rows) != 1 {
		return Effects{}, fmt.Errorf("load judgment %s: %w", judgmentID, err)
	}
	j := rows[0]
	outcomes, err := loadOutcomes(ctx, q, []uuid.UUID{judgmentID}, nil)
	if err != nil {
		return Effects{}, err
	}
	reviewed, err := exampleReviews(ctx, q, j.ExampleID)
	if err != nil {
		return Effects{}, err
	}

	siblings := make([]sibling, 0, len(outcomes))
	var pending []*outcomeRecord
	for _, o := range outcomes {
		value := o.value()
		if right, ok := reviewed[o.key()]; ok {
			value = right
		} else if o.class == nil && o.QuestionID != "" {
			pending = append(pending, o)
		}
		siblings = append(siblings, sibling{o.Question, contactOf(o.ContactID), value})
	}
	slices.SortStableFunc(pending, landingOrder)

	p := &planner{s: s, tx: tx, thread: thread, cutOff: cutOff, wants: wantsOf(siblings), openedNow: map[uuid.UUID]bool{}}
	locator := judgmentLocator(j.ID, MethodRef{j.MethodKey, j.MethodVersion, j.WordingVersion}, j.InputHash)
	var effects Effects
	land := func(outcomeID uuid.UUID, pl plan, value string, confidence int16) error {
		accepted, err := s.assertClassTx(ctx, tx, outcomeID, pl.class(), value, confidence, locator)
		if err != nil {
			return err
		}
		if pl.class() == predicateRemoval {
			if err := s.assertRemovalTargetTx(ctx, tx, outcomeID, pl.target(), locator); err != nil {
				return err
			}
		}
		if !accepted {
			effects.ClaimsRaised++
			return nil
		}
		opened, flags, err := p.apply(ctx, pl, locator, confidence)
		effects.ObligationsOpened += opened
		effects.FlagsRaised += flags
		return err
	}
	for _, o := range pending {
		pl, err := p.plan(ctx, o.Question, o.ContactID, o.value(), o.DueDate, "")
		if err != nil {
			return Effects{}, err
		}
		if err := land(o.ID, pl, o.value(), percent(o.confidence())); err != nil {
			return Effects{}, err
		}
	}

	// Omissions: an open obligation no outcome of this judgment speaks to.
	open, err := p.openObligations(ctx)
	if err != nil {
		return Effects{}, err
	}
	for _, task := range open {
		question, value := omissionOf(task)
		key := outcomeKey{question, task.ContactID}
		if _, ok := reviewed[key]; ok || mentions(outcomes, task) {
			continue
		}
		contact := task.ContactID
		pl, err := p.plan(ctx, question, &contact, value, nil, "")
		if err != nil {
			return Effects{}, err
		}
		if pl.class() != predicateRemoval {
			continue
		}
		omission, err := q.CreateJudgmentOutcome(ctx, db.CreateJudgmentOutcomeParams{
			JudgmentID: judgmentID, Question: string(question), ContactID: &contact,
		})
		if err != nil {
			return Effects{}, fmt.Errorf("create omission: %w", err)
		}
		if err := land(omission.NodeID, pl, value, 0); err != nil {
			return Effects{}, err
		}
	}
	return effects, nil
}

// landingOrder lands what opens an obligation before what only keeps one:
// commitments first, earliest date then highest confidence, so one party's
// commitments open one obligation at the earliest date; then owed replies by
// highest confidence, so the user's reply task belongs to the owed contact
// judged most confidently; then the rest.
func landingOrder(a, b *outcomeRecord) int {
	return cmp.Or(
		cmp.Compare(landingRank(a), landingRank(b)),
		compareDue(a.DueDate, b.DueDate),
		cmp.Compare(b.confidence(), a.confidence()),
	)
}

func landingRank(o *outcomeRecord) int {
	switch {
	case (o.Question == QuestionCommitmentMine || o.Question == QuestionCommitmentTheirs) && o.value() == string(CommitmentStateOpen):
		return 0
	case o.Question == QuestionBallInCourt && (o.value() == string(BallInCourtIOweReply) || o.value() == string(BallInCourtTheyOweReply)):
		return 1
	default:
		return 2
	}
}

func compareDue(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return a.Compare(*b)
	}
}

// omissionOf is the question and no-effect value whose absence from a
// judgment omits an open obligation.
func omissionOf(task repository.ContactTask) (Question, string) {
	switch {
	case task.Obligation.Basis == repository.ObligationBasisReply:
		return QuestionBallInCourt, string(BallInCourtNobodyOwes)
	case task.Obligation.Debtor == repository.ObligationDebtorUser:
		return QuestionCommitmentMine, string(CommitmentStateNone)
	default:
		return QuestionCommitmentTheirs, string(CommitmentStateNone)
	}
}

// mentions reports whether any of a judgment's outcomes speaks to an open
// obligation: for the user's, any outcome of its question; for a
// participant's, one of its question about that participant.
func mentions(outcomes []*outcomeRecord, task repository.ContactTask) bool {
	question, _ := omissionOf(task)
	user := task.Obligation.Debtor == repository.ObligationDebtorUser
	return slices.ContainsFunc(outcomes, func(o *outcomeRecord) bool {
		return o.Question == question && (user || contactOf(o.ContactID) == task.ContactID)
	})
}

// exampleReviews returns the user's latest right outcome per question and
// contact on an example, whichever method's outcome the user reviewed.
func exampleReviews(ctx context.Context, q db.Querier, exampleID string) (map[outcomeKey]string, error) {
	rows, err := q.ListLatestExampleReviews(ctx, db.ListLatestExampleReviewsParams{
		ReviewPredicate: predicateReview, ExampleIds: []string{exampleID},
	})
	if err != nil {
		return nil, fmt.Errorf("list example reviews: %w", err)
	}
	reviewed := make(map[outcomeKey]string, len(rows))
	for _, r := range rows {
		if r.RightOutcome != nil {
			reviewed[outcomeKey{Question(r.Question), contactOf(r.ContactID)}] = *r.RightOutcome
		}
	}
	return reviewed, nil
}

// assertClassTx records how an outcome landed and reports whether the catalog
// landed it accepted.
func (s *Store) assertClassTx(ctx context.Context, tx pgx.Tx, outcomeID uuid.UUID, class, value string, confidence int16, locator service.ProvenanceLocator) (bool, error) {
	a, err := s.assert.AssertTx(ctx, tx, service.AssertRequest{
		SubjectNodeID: outcomeID,
		PredicateKey:  class,
		ValueText:     &value,
		Confidence:    confidence,
		Locators:      []service.ProvenanceLocator{locator},
	})
	if err != nil {
		return false, fmt.Errorf("assert %s: %w", class, err)
	}
	return a.Status == repository.AssertionStatusAccepted, nil
}

// assertRemovalTargetTx records the live value a removal landing targets.
func (s *Store) assertRemovalTargetTx(ctx context.Context, tx pgx.Tx, outcomeID uuid.UUID, target string, locator service.ProvenanceLocator) error {
	if _, err := s.assert.AssertTx(ctx, tx, service.AssertRequest{
		SubjectNodeID: outcomeID,
		PredicateKey:  predicateRemovalTarget,
		ValueText:     &target,
		Confidence:    100,
		Locators:      []service.ProvenanceLocator{locator},
	}); err != nil {
		return fmt.Errorf("assert removal target: %w", err)
	}
	return nil
}

// sibling is one outcome's question, contact and effective value.
type sibling struct {
	question Question
	contact  uuid.UUID
	value    string
}

// wants is what a judgment's effective outcomes keep open: the user's
// obligation (owed reply or commitment), the user's commitment, and each
// participant's commitment. An outcome closes an obligation only when no
// sibling still wants it.
type wants struct {
	user              bool
	userCommitment    bool
	contactCommitment map[uuid.UUID]bool
}

func wantsOf(siblings []sibling) wants {
	w := wants{contactCommitment: map[uuid.UUID]bool{}}
	for _, s := range siblings {
		switch s.question {
		case QuestionBallInCourt:
			w.user = w.user || s.value == string(BallInCourtIOweReply)
		case QuestionCommitmentMine:
			if s.value == string(CommitmentStateOpen) {
				w.user, w.userCommitment = true, true
			}
		case QuestionCommitmentTheirs:
			if s.value == string(CommitmentStateOpen) {
				w.contactCommitment[s.contact] = true
			}
		}
	}
	return w
}

type actionKind int

const (
	actOpen actionKind = iota
	actClose
	actUpgrade
	actRedate
	actEndWait
	actRaiseFlag
	actWithdrawFlag
)

// action is one change an outcome makes to live state.
type action struct {
	kind    actionKind
	task    *repository.ContactTask
	debtor  string
	basis   string
	contact uuid.UUID
	due     string
	// later marks a redate or upgrade that moves an open obligation's date
	// later: a removal.
	later bool
	flag  string
}

// plan is the change one outcome makes to live state.
type plan struct{ actions []action }

func (p *plan) add(a action) { p.actions = append(p.actions, a) }

// class is the plan's landing class: a removal when it closes an obligation,
// ends a wait, moves an open obligation's date later (by a redate or by an
// upgrade) or withdraws a flag; an addition when it opens an obligation,
// upgrades one without delaying it, moves a date earlier or raises a flag;
// otherwise no effect.
func (p plan) class() string {
	addition := false
	for _, a := range p.actions {
		switch {
		case a.kind == actClose || a.kind == actEndWait || a.kind == actWithdrawFlag || a.later:
			return predicateRemoval
		default:
			addition = true
		}
	}
	if addition {
		return predicateAddition
	}
	return predicateNoEffect
}

// target is the outcome value whose live effect a removal plan takes away:
// the obligation it closes, the wait it ends, the flag it withdraws, or the
// open commitment it moves later.
func (p plan) target() string {
	for _, a := range p.actions {
		switch {
		case a.kind == actClose:
			return obligationValue(a.task)
		case a.kind == actEndWait:
			return string(BallInCourtTheyOweReply)
		case a.kind == actWithdrawFlag:
			return a.flag
		case a.later:
			return string(CommitmentStateOpen)
		}
	}
	return ""
}

// obligationValue is the outcome value whose effect an obligation is.
func obligationValue(task *repository.ContactTask) string {
	switch {
	case task.Obligation.Basis == repository.ObligationBasisCommitment:
		return string(CommitmentStateOpen)
	case task.Obligation.Debtor == repository.ObligationDebtorUser:
		return string(BallInCourtIOweReply)
	default:
		return string(BallInCourtTheyOweReply)
	}
}

// liveObligation is the open obligation of debtor on a thread: the user's one
// obligation whatever contact it is owed to, or the contact's.
func liveObligation(open []repository.ContactTask, debtor string, contact uuid.UUID) *repository.ContactTask {
	for i := range open {
		if open[i].Obligation.Debtor == debtor && (debtor == repository.ObligationDebtorUser || open[i].ContactID == contact) {
			return &open[i]
		}
	}
	return nil
}

// planner plans and applies outcomes' effects on one thread within one
// transaction.
type planner struct {
	s      *Store
	tx     pgx.Tx
	thread uuid.UUID
	// cutOff is recorded on an obligation the planner opens; a later message
	// closes a reply obligation.
	cutOff time.Time
	wants  wants
	// openedNow holds the obligations opened in this landing: a sibling's later
	// date does not move them.
	openedNow map[uuid.UUID]bool
}

func (p *planner) openObligations(ctx context.Context) ([]repository.ContactTask, error) {
	tasks, err := p.s.tasks.ListObligationTasksByThreadTx(ctx, p.tx, p.thread)
	if err != nil {
		return nil, fmt.Errorf("list obligations: %w", err)
	}
	return slices.DeleteFunc(tasks, func(t repository.ContactTask) bool { return !t.IsLive() }), nil
}

// plan computes the change an outcome of value makes to the thread's live
// state. withdraw names a big-moment flag the outcome raised before, which a
// different value withdraws.
func (p *planner) plan(ctx context.Context, question Question, contact *uuid.UUID, value string, due *time.Time, withdraw string) (plan, error) {
	open, err := p.openObligations(ctx)
	if err != nil {
		return plan{}, err
	}
	find := func(debtor string, c uuid.UUID) *repository.ContactTask { return liveObligation(open, debtor, c) }
	replyOpen := func(t *repository.ContactTask) bool {
		return t != nil && t.Obligation.Basis == repository.ObligationBasisReply
	}
	var pl plan
	switch question {
	case QuestionBallInCourt:
		c := *contact
		user, their := find(repository.ObligationDebtorUser, c), find(repository.ObligationDebtorContact, c)
		closeUser := replyOpen(user) && !p.wants.user
		closeTheirs := replyOpen(their) && !p.wants.contactCommitment[c]
		switch BallInCourt(value) {
		case BallInCourtIOweReply:
			if user == nil {
				pl.add(action{kind: actOpen, debtor: repository.ObligationDebtorUser, basis: repository.ObligationBasisReply, contact: c, due: today()})
			}
			if closeTheirs {
				pl.add(action{kind: actClose, task: their})
			}
		case BallInCourtTheyOweReply:
			if closeUser {
				pl.add(action{kind: actClose, task: user})
			}
			if their == nil {
				waiting, err := awaiting(ctx, db.New(p.tx), c)
				if err != nil {
					return plan{}, err
				}
				if !waiting {
					nudge, err := p.s.nudgeDueTx(ctx, p.tx, c)
					if err != nil {
						return plan{}, err
					}
					pl.add(action{kind: actOpen, debtor: repository.ObligationDebtorContact, basis: repository.ObligationBasisReply, contact: c, due: nudge})
				}
			}
		case BallInCourtNobodyOwes, BallInCourtNotApplicable:
			if closeUser {
				pl.add(action{kind: actClose, task: user})
			}
			if closeTheirs {
				pl.add(action{kind: actClose, task: their})
			}
			if BallInCourt(value) == BallInCourtNobodyOwes {
				started, err := p.s.waitStartedHereTx(ctx, p.tx, c, p.thread)
				if err != nil {
					return plan{}, err
				}
				if started {
					pl.add(action{kind: actEndWait, contact: c})
				}
			}
		}
	case QuestionCommitmentMine, QuestionCommitmentTheirs:
		c := *contact
		debtor, wanted := repository.ObligationDebtorUser, p.wants.userCommitment
		if question == QuestionCommitmentTheirs {
			debtor, wanted = repository.ObligationDebtorContact, p.wants.contactCommitment[c]
		}
		task := find(debtor, c)
		if value == string(CommitmentStateNone) {
			if task != nil && task.Obligation.Basis == repository.ObligationBasisCommitment && !wanted {
				pl.add(action{kind: actClose, task: task})
			}
			break
		}
		date := ""
		if due != nil {
			date = due.Format(dateFormat)
		}
		switch {
		case task == nil:
			if date == "" {
				if date, err = p.defaultDueTx(ctx, debtor, c); err != nil {
					return plan{}, err
				}
			}
			pl.add(action{kind: actOpen, debtor: debtor, basis: repository.ObligationBasisCommitment, contact: c, due: date})
		case task.Obligation.Basis == repository.ObligationBasisReply:
			// The upgraded obligation is the reply's, opened on the reply's day,
			// so without a selected date it keeps the reply's date; with one it
			// takes the selected date, and a later one delays the obligation.
			current := dueOf(task)
			if date == "" {
				date = current
			}
			pl.add(action{kind: actUpgrade, task: task, debtor: debtor, basis: repository.ObligationBasisCommitment, contact: c, due: date, later: date > current})
		case date != "" && date < dueOf(task):
			pl.add(action{kind: actRedate, task: task, due: date})
		case date != "" && date > dueOf(task) && !p.openedNow[task.ID]:
			pl.add(action{kind: actRedate, task: task, due: date, later: true})
		}
	case QuestionBigMoment:
		if withdraw != "" && withdraw != value && withdraw != string(BigMomentNone) {
			pl.add(action{kind: actWithdrawFlag, flag: withdraw})
		}
		if value != string(BigMomentNone) {
			raised, err := flagRaised(ctx, db.New(p.tx), p.thread, value)
			if err != nil {
				return plan{}, err
			}
			if !raised {
				pl.add(action{kind: actRaiseFlag, flag: value})
			}
		}
	}
	return pl, nil
}

// defaultDueTx is an obligation's due date when the outcome selected none: the
// day it opens for the user's, the contact's nudge date for a participant's.
func (p *planner) defaultDueTx(ctx context.Context, debtor string, contactID uuid.UUID) (string, error) {
	if debtor == repository.ObligationDebtorUser {
		return today(), nil
	}
	return p.s.nudgeDueTx(ctx, p.tx, contactID)
}

// apply makes a plan's changes and counts the obligations it opened and the
// flags it raised.
func (p *planner) apply(ctx context.Context, pl plan, locator service.ProvenanceLocator, confidence int16) (opened, flags int, err error) {
	for _, a := range pl.actions {
		switch a.kind {
		case actOpen, actUpgrade:
			if a.kind == actUpgrade {
				if err := p.s.closeTaskTx(ctx, p.tx, a.task); err != nil {
					return 0, 0, err
				}
			}
			task, err := p.s.openObligationTx(ctx, p.tx, p.thread, p.cutOff, a.debtor, a.basis, a.contact, a.due)
			if err != nil {
				return 0, 0, err
			}
			if task != nil {
				p.openedNow[task.ID] = true
				opened++
			}
		case actClose:
			if err := p.s.closeTaskTx(ctx, p.tx, a.task); err != nil {
				return 0, 0, err
			}
		case actRedate:
			if err := p.s.redateTaskTx(ctx, p.tx, a.task, a.due); err != nil {
				return 0, 0, err
			}
		case actEndWait:
			if err := p.s.endWaitTx(ctx, p.tx, a.contact); err != nil {
				return 0, 0, err
			}
		case actRaiseFlag:
			if err := p.s.raiseFlagTx(ctx, p.tx, p.thread, a.flag, confidence, locator); err != nil {
				return 0, 0, err
			}
			flags++
		case actWithdrawFlag:
			if err := p.s.withdrawFlagTx(ctx, p.tx, p.thread, a.flag); err != nil {
				return 0, 0, err
			}
		}
	}
	return opened, flags, nil
}

// openObligationTx opens an obligation with its one task. The task is due on
// due; its kind is what completing it records. It returns nil when the contact
// no longer exists. When Todoist is configured, the task's remote create is
// enqueued in the same transaction.
func (s *Store) openObligationTx(ctx context.Context, tx pgx.Tx, thread uuid.UUID, cutOff time.Time, debtor, basis string, contactID uuid.UUID, due string) (*repository.ContactTask, error) {
	contact, err := s.contacts.GetContactTx(ctx, tx, contactID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get contact for obligation: %w", err)
	}
	kind := contacttask.KindReachOut
	if debtor == repository.ObligationDebtorUser && basis == repository.ObligationBasisCommitment {
		kind = contacttask.KindReminder
	}
	settings, err := s.todoistSettingsTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	marker := contacttask.CRMMarker{ContactID: contactID.String(), Kind: kind, Lifecycle: contacttask.LifecycleObligation}
	if settings != nil {
		marker.Instance = settings.IntegrationInstanceID
	}
	markerJSON, err := contacttask.EncodeMarker(marker)
	if err != nil {
		return nil, fmt.Errorf("encode marker: %w", err)
	}
	metadata := map[string]any{
		"due_date":    due,
		"content":     obligationContent(debtor, basis, contact.FullName),
		"marker_json": string(markerJSON),
	}
	if settings != nil {
		metadata[todoist.MetadataKeyProjectID] = settings.ProjectID
		metadata[todoist.MetadataKeyLabelName] = settings.LabelName
		metadata[todoist.MetadataKeyIntegrationInstance] = settings.IntegrationInstanceID
	}
	task, err := s.tasks.CreateObligationTaskTx(ctx, tx, repository.CreateObligationTaskRequest{
		ContactID: contactID,
		Provider:  todoist.SourceName,
		Kind:      kind,
		State:     repository.ContactTaskStatePendingRemoteCreate,
		Metadata:  metadata,
		Obligation: repository.TaskObligation{
			ThreadID: thread, Debtor: debtor, Basis: basis, CutOff: cutOff,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("open obligation: %w", err)
	}
	if settings != nil {
		if err := s.enqueueTaskOp(ctx, tx, task.ID, consumerjobs.TaskOpCreate); err != nil {
			return nil, err
		}
	}
	return task, nil
}

// obligationContent is an obligation task's title.
func obligationContent(debtor, basis, name string) string {
	switch {
	case debtor == repository.ObligationDebtorUser && basis == repository.ObligationBasisReply:
		return "Reply to " + name
	case debtor == repository.ObligationDebtorUser:
		return "Follow through on what you told " + name
	case basis == repository.ObligationBasisReply:
		return "Nudge " + name + " (awaiting their reply)"
	default:
		return "Nudge " + name + " (they said they would)"
	}
}

// closeTaskTx ends a live task: it completes it locally and enqueues the
// remote close once the task exists remotely (a task still awaiting its remote
// create is closed by the create's finalize).
func (s *Store) closeTaskTx(ctx context.Context, tx pgx.Tx, task *repository.ContactTask) error {
	updated, err := s.tasks.UpdateContactTaskStateTx(ctx, tx, task.ID, repository.ContactTaskStateCompleted)
	if err != nil {
		return fmt.Errorf("close task %s: %w", task.ID, err)
	}
	if updated.ExternalTaskID == "" {
		return nil
	}
	return s.enqueueTaskOp(ctx, tx, updated.ID, consumerjobs.TaskOpClose)
}

// redateTaskTx moves a task's due date, locally and through the durable
// update op when the task is Todoist-backed.
func (s *Store) redateTaskTx(ctx context.Context, tx pgx.Tx, task *repository.ContactTask, due string) error {
	metadata := make(map[string]any, len(task.Metadata)+1)
	for k, v := range task.Metadata {
		metadata[k] = v
	}
	metadata["due_date"] = due
	if _, err := s.tasks.UpdateContactTaskMetadataTx(ctx, tx, task.ID, metadata); err != nil {
		return fmt.Errorf("redate task %s: %w", task.ID, err)
	}
	if label, _ := metadata[todoist.MetadataKeyLabelName].(string); label == "" {
		return nil
	}
	return s.enqueueTaskOp(ctx, tx, task.ID, consumerjobs.TaskOpUpdateDeadline)
}

// endWaitTx ends the contact's awaiting-reply wait through the cadence owner
// and closes the follow-up reminder that tracked it.
func (s *Store) endWaitTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) error {
	if err := s.cadence.EndAwaitingReplyTx(ctx, tx, contactID); err != nil {
		return fmt.Errorf("end awaiting-reply wait: %w", err)
	}
	followUp, err := s.tasks.FindPendingFollowUpTx(ctx, tx, contactID)
	if errors.Is(err, db.ErrNotFound) || followUp == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find live follow-up: %w", err)
	}
	return s.closeTaskTx(ctx, tx, followUp)
}

func (s *Store) enqueueTaskOp(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, op string) error {
	if _, err := s.jobs.InsertTx(ctx, tx,
		consumerjobs.TodoistTaskOpArgs{ContactTaskID: taskID, Op: op},
		&river.InsertOpts{MaxAttempts: 10}); err != nil {
		return fmt.Errorf("enqueue todoist %s op: %w", op, err)
	}
	return nil
}

// awaiting reports whether the timestamp rule shows the contact awaiting a
// reply (CAD-040).
func awaiting(ctx context.Context, q db.Querier, contactID uuid.UUID) (bool, error) {
	contact, err := repository.NewContactRepository(q).GetContact(ctx, contactID)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get contact: %w", err)
	}
	return cadence.IsAwaitingReply(contact.LastOutreachAt, contact.LastResponseAt, contact.AwaitingReplyUntil, now()), nil
}

// waitStartedHereTx reports whether the contact is awaiting a reply and the
// outreach that started the wait happened on the thread.
func (s *Store) waitStartedHereTx(ctx context.Context, tx pgx.Tx, contactID, thread uuid.UUID) (bool, error) {
	waiting, err := awaiting(ctx, db.New(tx), contactID)
	if err != nil || !waiting {
		return false, err
	}
	started, err := db.New(tx).InteractionStartedAwaitingWait(ctx, db.InteractionStartedAwaitingWaitParams{
		ContactID: contactID, VenueID: &thread,
	})
	if err != nil {
		return false, fmt.Errorf("find the wait's outreach: %w", err)
	}
	return started, nil
}

// nudgeDueTx is when a participant's obligation without a selected date is
// due: the contact's cadence watchdog days after today, else seven days.
func (s *Store) nudgeDueTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) (string, error) {
	due, err := s.nudgeDateTx(ctx, tx, contactID)
	if err != nil {
		return "", err
	}
	return due.Format(dateFormat), nil
}

// nudgeDateTx is the date nudgeDueTx formats.
func (s *Store) nudgeDateTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) (time.Time, error) {
	contact, err := s.contacts.GetContactTx(ctx, tx, contactID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return time.Time{}, fmt.Errorf("get contact: %w", err)
	}
	days := 0
	if contact != nil && contact.Cadence != nil {
		days = s.watchdog.DaysForCadence(*contact.Cadence)
	}
	if days == 0 {
		days = defaultNudgeDays
	}
	return cadence.Today(now()).AddDate(0, 0, days), nil
}

// flagRaised reports whether the thread carries the live flag.
func flagRaised(ctx context.Context, q db.Querier, thread uuid.UUID, flag string) (bool, error) {
	_, err := q.FindLiveVenueFlag(ctx, db.FindLiveVenueFlagParams{VenueID: thread, PredicateKey: predicateFlag, Value: flag})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find flag: %w", err)
	}
	return true, nil
}

func (s *Store) raiseFlagTx(ctx context.Context, tx pgx.Tx, thread uuid.UUID, flag string, confidence int16, locator service.ProvenanceLocator) error {
	if _, err := s.assert.AssertTx(ctx, tx, service.AssertRequest{
		SubjectNodeID: thread,
		PredicateKey:  predicateFlag,
		ValueText:     &flag,
		Confidence:    confidence,
		Locators:      []service.ProvenanceLocator{locator},
	}); err != nil {
		return fmt.Errorf("raise flag: %w", err)
	}
	return nil
}

func (s *Store) withdrawFlagTx(ctx context.Context, tx pgx.Tx, thread uuid.UUID, flag string) error {
	id, err := db.New(tx).FindLiveVenueFlag(ctx, db.FindLiveVenueFlagParams{VenueID: thread, PredicateKey: predicateFlag, Value: flag})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find flag: %w", err)
	}
	if _, err := s.assert.RetractTx(ctx, tx, id); err != nil {
		return fmt.Errorf("withdraw flag: %w", err)
	}
	return nil
}

// todoistSettings is what a new task's metadata snapshots from the Todoist
// sync state.
type todoistSettings struct {
	ProjectID             string
	LabelName             string
	IntegrationInstanceID string
}

// todoistSettingsTx returns the Todoist settings, or nil when Todoist is not
// configured (no sync state, or no label).
func (s *Store) todoistSettingsTx(ctx context.Context, tx pgx.Tx) (*todoistSettings, error) {
	raw, err := db.New(tx).GetTodoistTaskSettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get todoist settings: %w", err)
	}
	var metadata map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, fmt.Errorf("decode todoist settings: %w", err)
		}
	}
	str := func(key string) string { v, _ := metadata[key].(string); return v }
	if str(todoist.MetadataKeyLabelID) == "" {
		return nil, nil
	}
	return &todoistSettings{
		ProjectID:             str(todoist.MetadataKeyProjectID),
		LabelName:             str(todoist.MetadataKeyLabelName),
		IntegrationInstanceID: str(todoist.MetadataKeyIntegrationInstance),
	}, nil
}

func dueOf(task *repository.ContactTask) string {
	due, _ := task.Metadata["due_date"].(string)
	return due
}

func today() string { return cadence.Today(now()).Format(dateFormat) }
