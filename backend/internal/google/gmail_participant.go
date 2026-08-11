package google

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ParticipantSource is the external_contact.source tag for candidates the
// trust-anchored participant-discovery gate proposes. Unlike
// CorrespondenceSource it is deliberately NOT a link-only source (absent from
// linkOnlySources): gmail_participant proposes brand-new contacts from To/Cc
// participants of messages sent by an already-trusted sender.
const ParticipantSource = "gmail_participant"

// buildParticipantEvidence assembles the evidence metadata for a
// gmail_participant candidate: message count, recency, observed display
// names, the trusted sender that anchored the message, and the anchor
// message's subject. A GetContact failure on the anchor sender's contact id
// degrades to address-only trusted_sender evidence rather than dropping the
// candidate (evidence gaps are cheap per the spec's severity grading).
func buildParticipantEvidence(ctx context.Context, contactRepo correspondenceContactRepo, agg *correspondenceAggregate) map[string]any {
	metadata := map[string]any{
		"message_count": agg.messageCount,
	}
	if agg.lastMessageEpoch > 0 {
		metadata["last_message_at"] = formatEvidenceTime(agg.lastMessageEpoch)
	}
	if len(agg.namesSeen) > 0 {
		metadata["display_names_seen"] = agg.namesSeen
	}

	trustedSender := map[string]any{"address": agg.anchorSenderNorm}
	switch {
	case agg.anchorSenderSelf:
		trustedSender["self"] = true
	case agg.anchorSenderContactID != uuid.Nil:
		if contact, err := contactRepo.GetContact(ctx, agg.anchorSenderContactID); err == nil && contact != nil {
			trustedSender["name"] = contact.FullName
		}
	}
	metadata["trusted_sender"] = trustedSender

	if agg.anchorSubject != "" {
		metadata["anchor_subject"] = agg.anchorSubject
	}
	return metadata
}

// formatEvidenceTime formats a passed-in epoch (never time.Now()) in the same
// UTC format the whatsapp/telegram matchers use for last_message_at.
func formatEvidenceTime(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}
