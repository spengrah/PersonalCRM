package todoist

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"personal-crm/backend/internal/logger"

	"github.com/google/uuid"
)

// Todoist Sync API endpoint
var SyncEndpoint = "https://api.todoist.com/sync/v9/sync"

// SyncClient handles Todoist Sync API operations
type SyncClient struct {
	httpClient  *http.Client
	accessToken string
}

// NewSyncClient creates a new Todoist Sync API client
func NewSyncClient(accessToken string) *SyncClient {
	return &SyncClient{
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		accessToken: accessToken,
	}
}

// SetHTTPClient allows setting a custom HTTP client (useful for testing)
func (c *SyncClient) SetHTTPClient(client *http.Client) {
	c.httpClient = client
}

// SyncRequest represents a request to the Sync API
type SyncRequest struct {
	SyncToken     string        `json:"sync_token"`
	ResourceTypes []string      `json:"resource_types"`
	Commands      []SyncCommand `json:"commands,omitempty"`
}

// SyncCommand represents a command in the Sync API
type SyncCommand struct {
	Type   string         `json:"type"`
	UUID   string         `json:"uuid"`
	TempID string         `json:"temp_id,omitempty"`
	Args   map[string]any `json:"args"`
}

// SyncResponse represents a response from the Sync API
type SyncResponse struct {
	SyncToken  string            `json:"sync_token"`
	FullSync   bool              `json:"full_sync"`
	Items      []SyncItem        `json:"items,omitempty"`
	Labels     []SyncLabel       `json:"labels,omitempty"`
	Projects   []SyncProject     `json:"projects,omitempty"`
	User       *SyncUser         `json:"user,omitempty"`
	SyncStatus map[string]any    `json:"sync_status,omitempty"`
	TempIDMap  map[string]string `json:"temp_id_mapping,omitempty"`
}

// SyncItem represents a task item from the Sync API
type SyncItem struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Content     string    `json:"content"`
	Description string    `json:"description"`
	Due         *SyncDue  `json:"due,omitempty"`
	Deadline    *SyncDate `json:"deadline,omitempty"`
	Labels      []string  `json:"labels"`
	Checked     bool      `json:"checked"`
	CompletedAt *string   `json:"completed_at,omitempty"`
	IsDeleted   bool      `json:"is_deleted"`
	UpdatedAt   string    `json:"updated_at"`
}

// SyncDue represents due information for a task
type SyncDue struct {
	Date        string `json:"date"`
	Timezone    string `json:"timezone,omitempty"`
	String      string `json:"string,omitempty"`
	Lang        string `json:"lang,omitempty"`
	IsRecurring bool   `json:"is_recurring"`
}

// SyncDate represents a deadline date
type SyncDate struct {
	Date     string `json:"date"`
	Timezone string `json:"timezone,omitempty"`
	Lang     string `json:"lang,omitempty"`
}

// SyncLabel represents a label from the Sync API
type SyncLabel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	IsDeleted bool   `json:"is_deleted"`
}

// SyncProject represents a project from the Sync API
type SyncProject struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	IsDeleted bool   `json:"is_deleted"`
}

// SyncUser represents user information from the Sync API
type SyncUser struct {
	ID       string  `json:"id"`
	Email    string  `json:"email"`
	FullName string  `json:"full_name"`
	Timezone string  `json:"tz_info,omitempty"`
	TzInfo   *TzInfo `json:"tz_info_object,omitempty"`
}

// TzInfo represents timezone information
type TzInfo struct {
	Timezone string `json:"timezone"`
}

// SyncError represents an error response from the Sync API
type SyncError struct {
	ErrorTag   string `json:"error_tag"`
	ErrorCode  int    `json:"error_code"`
	HTTPCode   int    `json:"http_code"`
	Error      string `json:"error"`
	ErrorExtra *struct {
		EventID    string `json:"event_id,omitempty"`
		RetryAfter int    `json:"retry_after,omitempty"`
	} `json:"error_extra,omitempty"`
}

// CommandError represents an error for a specific command
type CommandError struct {
	ErrorTag  string `json:"error_tag"`
	ErrorCode int    `json:"error_code"`
	Error     string `json:"error"`
}

// Sync performs a sync operation with the Todoist Sync API
func (c *SyncClient) Sync(ctx context.Context, syncToken string, resourceTypes []string, commands []SyncCommand) (*SyncResponse, error) {
	// Build request body as form data (Todoist uses x-www-form-urlencoded)
	data := url.Values{}
	data.Set("sync_token", syncToken)
	data.Set("resource_types", toJSONArray(resourceTypes))

	if len(commands) > 0 {
		commandsJSON, err := json.Marshal(commands)
		if err != nil {
			return nil, fmt.Errorf("marshal commands: %w", err)
		}
		data.Set("commands", string(commandsJSON))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, SyncEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("X-Request-Id", uuid.New().String())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sync request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warn().Err(err).Msg("failed to close sync response body")
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	// Handle error responses
	if resp.StatusCode != http.StatusOK {
		return nil, c.handleErrorResponse(resp.StatusCode, body)
	}

	var syncResp SyncResponse
	if err := json.Unmarshal(body, &syncResp); err != nil {
		return nil, fmt.Errorf("decode sync response: %w", err)
	}

	// Check for command-level errors in sync_status
	if len(commands) > 0 && len(syncResp.SyncStatus) > 0 {
		c.logCommandErrors(commands, syncResp.SyncStatus)
	}

	return &syncResp, nil
}

// handleErrorResponse processes error responses from the Sync API
func (c *SyncClient) handleErrorResponse(statusCode int, body []byte) error {
	// Try to parse as SyncError
	var syncErr SyncError
	if err := json.Unmarshal(body, &syncErr); err == nil && syncErr.Error != "" {
		logger.Error().
			Str("provider", "todoist").
			Str("endpoint", "sync").
			Int("http_status", statusCode).
			Str("error_tag", syncErr.ErrorTag).
			Int("error_code", syncErr.ErrorCode).
			Msg("Todoist API error")

		return &APIError{
			StatusCode: statusCode,
			ErrorTag:   syncErr.ErrorTag,
			ErrorCode:  syncErr.ErrorCode,
			Message:    syncErr.Error,
			RetryAfter: c.getRetryAfter(syncErr),
		}
	}

	// Generic error response
	logger.Error().
		Str("provider", "todoist").
		Str("endpoint", "sync").
		Int("http_status", statusCode).
		Str("body", string(body)).
		Msg("Todoist API error")

	return &APIError{
		StatusCode: statusCode,
		Message:    fmt.Sprintf("sync request failed with status %d: %s", statusCode, string(body)),
	}
}

// logCommandErrors logs errors for individual commands
func (c *SyncClient) logCommandErrors(commands []SyncCommand, syncStatus map[string]any) {
	for _, cmd := range commands {
		status, ok := syncStatus[cmd.UUID]
		if !ok {
			continue
		}

		// Check if status indicates an error (not "ok")
		if statusStr, ok := status.(string); ok && statusStr == "ok" {
			continue
		}

		// Try to parse as error object
		statusJSON, _ := json.Marshal(status)
		logger.Error().
			Str("provider", "todoist").
			Str("command_type", cmd.Type).
			Str("command_uuid", cmd.UUID).
			RawJSON("sync_status", statusJSON).
			Msg("Todoist command error")
	}
}

// getRetryAfter extracts retry-after value from error response
func (c *SyncClient) getRetryAfter(syncErr SyncError) int {
	if syncErr.ErrorExtra != nil && syncErr.ErrorExtra.RetryAfter > 0 {
		return syncErr.ErrorExtra.RetryAfter
	}
	return 0
}

// APIError represents a Todoist API error
type APIError struct {
	StatusCode int
	ErrorTag   string
	ErrorCode  int
	Message    string
	RetryAfter int
}

func (e *APIError) Error() string {
	if e.ErrorTag != "" {
		return fmt.Sprintf("todoist API error (%d): %s - %s", e.StatusCode, e.ErrorTag, e.Message)
	}
	return fmt.Sprintf("todoist API error (%d): %s", e.StatusCode, e.Message)
}

// IsAuthError returns true if the error indicates an authentication problem
func (e *APIError) IsAuthError() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// IsRateLimitError returns true if the error indicates rate limiting
func (e *APIError) IsRateLimitError() bool {
	return e.StatusCode == http.StatusTooManyRequests
}

// IsTransientError returns true if the error is transient (network, server errors)
func (e *APIError) IsTransientError() bool {
	return e.StatusCode >= 500 || e.StatusCode == http.StatusTooManyRequests
}

// Command helpers

// NewItemAddCommand creates a command to add a new task
func NewItemAddCommand(content, description, projectID string, labels []string, deadline *string) SyncCommand {
	args := map[string]any{
		"content": content,
	}
	if description != "" {
		args["description"] = description
	}
	if projectID != "" {
		args["project_id"] = projectID
	}
	if len(labels) > 0 {
		args["labels"] = labels
	}
	if deadline != nil {
		args["deadline"] = map[string]string{"date": *deadline}
	}

	return SyncCommand{
		Type:   "item_add",
		UUID:   uuid.New().String(),
		TempID: uuid.New().String(),
		Args:   args,
	}
}

// NewItemUpdateCommand creates a command to update a task
func NewItemUpdateCommand(taskID string, updates map[string]any) SyncCommand {
	args := map[string]any{"id": taskID}
	for k, v := range updates {
		args[k] = v
	}

	return SyncCommand{
		Type: "item_update",
		UUID: uuid.New().String(),
		Args: args,
	}
}

// NewItemCloseCommand creates a command to complete a task
func NewItemCloseCommand(taskID string) SyncCommand {
	return SyncCommand{
		Type: "item_close",
		UUID: uuid.New().String(),
		Args: map[string]any{"id": taskID},
	}
}

// NewItemDeleteCommand creates a command to delete a task
func NewItemDeleteCommand(taskID string) SyncCommand {
	return SyncCommand{
		Type: "item_delete",
		UUID: uuid.New().String(),
		Args: map[string]any{"id": taskID},
	}
}

// toJSONArray converts a string slice to a JSON array string
func toJSONArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	result, _ := json.Marshal(items)
	return string(result)
}

// BatchCommands splits commands into batches of maxSize
func BatchCommands(commands []SyncCommand, maxSize int) [][]SyncCommand {
	if maxSize <= 0 {
		maxSize = 100 // Todoist limit
	}

	var batches [][]SyncCommand
	for i := 0; i < len(commands); i += maxSize {
		end := i + maxSize
		if end > len(commands) {
			end = len(commands)
		}
		batches = append(batches, commands[i:end])
	}

	return batches
}
