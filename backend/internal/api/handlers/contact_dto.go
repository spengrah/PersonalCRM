package handlers

import "time"

// Contact wire DTOs. This file is the tygo source for the generated frontend
// types (backend/tygo.yaml → frontend/src/types/generated/contact.ts): only
// JSON payload structs belong here — handler structs, form/query binding
// structs, and helpers stay in contact.go. The `tstype` tags are
// TypeScript-generation metadata only; Go ignores them.

// Contact response model
// @Description Contact information
type ContactResponse struct {
	ID                 string                  `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	FullName           string                  `json:"full_name" example:"John Doe"`
	Methods            []ContactMethodResponse `json:"methods,omitempty"`
	PrimaryMethod      *ContactMethodResponse  `json:"primary_method,omitempty"`
	Location           *string                 `json:"location,omitempty" example:"San Francisco, CA"`
	Birthday           *time.Time              `json:"birthday,omitempty" example:"1990-01-15T00:00:00Z"`
	HowMet             *string                 `json:"how_met,omitempty" example:"Met at tech conference"`
	Cadence            *string                 `json:"cadence,omitempty" example:"monthly" enums:"weekly,monthly,quarterly,biannual,annual"`
	LastContacted      *time.Time              `json:"last_contacted,omitempty" example:"2024-01-15T10:30:00Z"`
	ContactBy          *time.Time              `json:"contact_by,omitempty" example:"2024-02-15T00:00:00Z"`
	LastInteractionAt  *time.Time              `json:"last_interaction_at,omitempty"`
	LastOutreachAt     *time.Time              `json:"last_outreach_at,omitempty"`
	LastResponseAt     *time.Time              `json:"last_response_at,omitempty"`
	HasPendingFollowup bool                    `json:"has_pending_followup"`
	ProfilePhoto       *string                 `json:"profile_photo,omitempty" example:"https://example.com/photo.jpg"`
	CreatedAt          time.Time               `json:"created_at" example:"2024-01-01T00:00:00Z"`
	UpdatedAt          time.Time               `json:"updated_at" example:"2024-01-15T10:30:00Z"`
	RematchJobID       *string                 `json:"rematch_job_id,omitempty"`
}

// ContactMethodResponse represents a contact method in responses
type ContactMethodResponse struct {
	ID        string `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	Type      string `json:"type" example:"email" tstype:"ContactMethodType"`
	Value     string `json:"value" example:"john.doe@example.com"`
	IsPrimary bool   `json:"is_primary" example:"true"`
}

// OverdueContactResponse represents an overdue contact with additional metadata
// @Description Overdue contact information with action metadata
type OverdueContactResponse struct {
	ContactResponse `tstype:",extends"`
	DaysOverdue     int       `json:"days_overdue" example:"5"`
	NextDueDate     time.Time `json:"next_due_date" example:"2024-01-15T00:00:00Z"`
	SuggestedAction string    `json:"suggested_action" example:"Send a quick check-in message"`
}

// CreateContactRequest represents the request to create a contact
// @Description Create contact request
type CreateContactRequest struct {
	FullName     string                 `json:"full_name" validate:"required,min=1,max=255" example:"John Doe"`
	Methods      []ContactMethodRequest `json:"methods,omitempty" validate:"omitempty,dive"`
	Location     *string                `json:"location,omitempty" validate:"omitempty,max=255" example:"San Francisco, CA"`
	Birthday     *DateOnly              `json:"birthday,omitempty" example:"1990-01-15" tstype:"string"`
	HowMet       *string                `json:"how_met,omitempty" validate:"omitempty,max=500" example:"Met at tech conference"`
	Cadence      *string                `json:"cadence,omitempty" validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual" example:"monthly"`
	ProfilePhoto *string                `json:"profile_photo,omitempty" validate:"omitempty,url,max=500" example:"https://example.com/photo.jpg"`
}

// UpdateContactRequest represents the request to update a contact
// @Description Update contact request
type UpdateContactRequest struct {
	FullName     string                 `json:"full_name" validate:"required,min=1,max=255" example:"John Doe"`
	Methods      []ContactMethodRequest `json:"methods,omitempty" validate:"omitempty,dive"`
	Location     *string                `json:"location,omitempty" validate:"omitempty,max=255" example:"San Francisco, CA"`
	Birthday     *DateOnly              `json:"birthday,omitempty" example:"1990-01-15" tstype:"string"`
	HowMet       *string                `json:"how_met,omitempty" validate:"omitempty,max=500" example:"Met at tech conference"`
	Cadence      *string                `json:"cadence,omitempty" validate:"omitempty,oneof=weekly biweekly monthly quarterly biannual annual" example:"monthly"`
	ProfilePhoto *string                `json:"profile_photo,omitempty" validate:"omitempty,url,max=500" example:"https://example.com/photo.jpg"`
}

// ContactIDsResponse represents the response for ID-only queries
type ContactIDsResponse struct {
	IDs   []string `json:"ids"`
	Total int      `json:"total"`
}

// ContactMethodRequest represents a single contact method in requests
type ContactMethodRequest struct {
	// Note: legacy email_personal/email_work are no longer accepted; clients should send "email".
	Type      string `json:"type" validate:"required,oneof=email phone telegram discord twitter signal gchat whatsapp" example:"email" tstype:"ContactMethodType"`
	Value     string `json:"value" validate:"required,max=255" example:"john.doe@example.com"`
	IsPrimary bool   `json:"is_primary" example:"true"`
}

// MergeContactsRequest represents the request to merge two contacts
// @Description Merge contacts request
type MergeContactsRequest struct {
	SourceContactID string                      `json:"source_contact_id" validate:"required,uuid" example:"550e8400-e29b-41d4-a716-446655440001"`
	FieldSelections MergeFieldSelectionsRequest `json:"field_selections,omitempty"`
	NewName         *string                     `json:"new_name,omitempty" validate:"omitempty,min=1,max=255" example:"John Smith"`
}

// MergeFieldSelectionsRequest specifies which contact's value to use for each field
type MergeFieldSelectionsRequest struct {
	Cadence  string `json:"cadence,omitempty" validate:"omitempty,oneof=source target" example:"target"`
	Location string `json:"location,omitempty" validate:"omitempty,oneof=source target" example:"source"`
	Birthday string `json:"birthday,omitempty" validate:"omitempty,oneof=source target" example:"target"`
}

// MergePreviewResponse represents the preview of a merge operation
// @Description Merge preview response
type MergePreviewResponse struct {
	SourceContact          ContactResponse `json:"source_contact"`
	TargetContact          ContactResponse `json:"target_contact"`
	MethodsToTransfer      int64           `json:"methods_to_transfer" example:"3"`
	DuplicateMethods       int64           `json:"duplicate_methods" example:"1"`
	NotesToTransfer        int64           `json:"notes_to_transfer" example:"5"`
	InteractionsToTransfer int64           `json:"interactions_to_transfer" example:"12"`
	CalendarEventsToUpdate int64           `json:"calendar_events_to_update" example:"3"`
}
