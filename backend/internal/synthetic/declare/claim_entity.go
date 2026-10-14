package declare

import (
	"context"
	"fmt"
	"strings"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
)

// messageClaimPlan is an extractor's proposed claim about an earlier message
// interaction's contact, whose provenance cites that interaction's first
// message.
type messageClaimPlan struct {
	name, interaction string
}

func (p *messageClaimPlan) handle() string { return p.name }
func (p *messageClaimPlan) kind() string   { return "assertion" }
func (p *messageClaimPlan) refs() []string { return []string{p.interaction} }
func (p *messageClaimPlan) refKind(string) string {
	return "interaction"
}
func (p *messageClaimPlan) validate() error {
	if strings.TrimSpace(p.name) == "" {
		return fmt.Errorf("message claim handle must be non-empty")
	}
	if strings.TrimSpace(p.interaction) == "" {
		return fmt.Errorf("message claim %q: interaction handle must be non-empty", p.name)
	}
	return nil
}

// MessageClaim declares a proposed preference claim about an earlier email,
// Google Chat or WhatsApp MessageInteraction's contact, citing its first
// message. It is lowered through the production assertion write path. The value
// is namespace-prefixed; a test reads it back from the API.
func MessageClaim(handle, interactionHandle string) Entity {
	return &messageClaimPlan{name: handle, interaction: interactionHandle}
}

func runMessageClaim(ctx context.Context, h *replay.Harness, p *messageClaimPlan, st *runState) (Seeded, error) {
	seeded, ok := st.seeded[p.interaction]
	if !ok {
		return Seeded{}, fmt.Errorf("message claim %q: handle %q has not been created", p.name, p.interaction)
	}
	interactionID, err := uuid.Parse(seeded.ID)
	if err != nil {
		return Seeded{}, fmt.Errorf("message claim %q: %w", p.name, err)
	}
	interaction, err := h.InteractionRepo().GetInteraction(ctx, interactionID)
	if err != nil {
		return Seeded{}, fmt.Errorf("message claim %q: read interaction: %w", p.name, err)
	}
	messages, err := h.CommsRepo().ListByInteractionIDs(ctx, []uuid.UUID{interactionID})
	if err != nil {
		return Seeded{}, fmt.Errorf("message claim %q: read messages: %w", p.name, err)
	}
	if len(messages) == 0 {
		return Seeded{}, fmt.Errorf("message claim %q: interaction %q has no email, Google Chat or WhatsApp message to cite", p.name, p.interaction)
	}
	value := h.Generator().Prefix() + "claim-" + p.name
	claim, err := h.ReplayClaim(ctx, interaction.ContactID, "preference", value, repository.SourceKindCommsMessage, messages[0].ID)
	if err != nil {
		return Seeded{}, fmt.Errorf("message claim %q: %w", p.name, err)
	}
	return Seeded{Kind: "assertion", ID: claim.AssertionID.String(), Name: value}, nil
}
