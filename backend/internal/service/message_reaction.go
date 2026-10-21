package service

import (
	"errors"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
)

// ReactionFacts converts a wire reaction object into the facts the
// messages_message row stores, or reports why it is not a valid reaction. Ingest
// stores a message whose object is absent or invalid with no facts; the
// enrichment exchange rejects a request that holds an invalid one.
func ReactionFacts(r *events.MessageReaction) (*repository.MessageReaction, error) {
	if r == nil {
		return nil, errors.New("reaction is required")
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &repository.MessageReaction{Kind: r.Kind, Emoji: r.Emoji, TargetKey: r.TargetKey, Removal: *r.Removal}, nil
}
