package copilot

import (
	"encoding/json"
	"time"
)

// CloudRepository describes the repository context used when creating a cloud
// sandbox task. Branch is optional.
type CloudRepository struct {
	Owner  string `json:"owner"`
	Name   string `json:"name"`
	Branch string `json:"branch,omitempty"`
}

// CloudProgressPhase represents a progress phase emitted while creating or
// attaching to a cloud sandbox session.
type CloudProgressPhase string

const (
	CloudProgressCreatingTask       CloudProgressPhase = "creating_task"
	CloudProgressProvisioningSandbox CloudProgressPhase = "provisioning_sandbox"
	CloudProgressWaitingForSession  CloudProgressPhase = "waiting_for_session"
	CloudProgressConnected          CloudProgressPhase = "connected"
)

// CloudProgressEvent is emitted during cloud session creation to report
// progress to the caller.
type CloudProgressEvent struct {
	Phase     CloudProgressPhase `json:"phase"`
	ElapsedMs int64              `json:"elapsedMs,omitempty"`
	TaskID    string             `json:"taskId,omitempty"`
}

// CloudSessionFailureReason categorises the cause of a cloud session error.
type CloudSessionFailureReason string

const (
	CloudFailurePolicyBlocked CloudSessionFailureReason = "policy_blocked"
	CloudFailureValidation    CloudSessionFailureReason = "validation"
	CloudFailureTimeout       CloudSessionFailureReason = "timeout"
	CloudFailureNetwork       CloudSessionFailureReason = "network"
	CloudFailureServer        CloudSessionFailureReason = "server"
)

// MissionControlTaskSession describes a single session within a Mission Control task.
type MissionControlTaskSession struct {
	ID          string  `json:"id"`
	TaskID      string  `json:"task_id"`
	AgentTaskID *string `json:"agent_task_id,omitempty"`
	State       string  `json:"state"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	Name        *string `json:"name,omitempty"`
	OwnerID     int     `json:"owner_id"`
	RepoID      *int    `json:"repo_id,omitempty"`
}

// MissionControlTask is the top-level task object returned by Mission Control.
type MissionControlTask struct {
	ID           string                      `json:"id"`
	Name         string                      `json:"name"`
	State        string                      `json:"state"`
	Status       string                      `json:"status"`
	CreatorID    int                         `json:"creator_id"`
	OwnerID      int                         `json:"owner_id"`
	RepoID       *int                        `json:"repo_id,omitempty"`
	SessionCount int                         `json:"session_count"`
	CreatedAt    string                      `json:"created_at"`
	UpdatedAt    string                      `json:"updated_at"`
	Sessions     []MissionControlTaskSession `json:"sessions,omitempty"`
}

// CloudSessionMetadata carries the metadata associated with a cloud session.
type CloudSessionMetadata struct {
	TaskID                    string           `json:"taskId"`
	MissionControlSessionID   string           `json:"missionControlSessionId,omitempty"`
	FrontendURL               string           `json:"frontendUrl"`
	Owner                     string           `json:"owner,omitempty"`
	Repository                *CloudRepository `json:"repository,omitempty"`
	CreatedAt                 time.Time        `json:"createdAt"`
	UpdatedAt                 time.Time        `json:"updatedAt"`
	State                     string           `json:"state,omitempty"`
	Status                    string           `json:"status,omitempty"`
}

// CloudSessionEvent represents a single event received from Mission Control's
// task event stream. The Data field holds arbitrary event payload data.
type CloudSessionEvent struct {
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
	Ephemeral *bool           `json:"ephemeral,omitempty"`
}

// MissionControlCommandType enumerates the steering command types accepted
// by the Mission Control steer API.
type MissionControlCommandType string

const (
	CommandUserMessage        MissionControlCommandType = "user_message"
	CommandAskUserResponse    MissionControlCommandType = "ask_user_response"
	CommandPlanApproval       MissionControlCommandType = "plan_approval_response"
	CommandPermissionResponse MissionControlCommandType = "permission_response"
	CommandElicitation        MissionControlCommandType = "elicitation_response"
	CommandAbort              MissionControlCommandType = "abort"
	CommandModeSwitch         MissionControlCommandType = "mode_switch"
)

// CloudAskUserResponsePayload is the payload for an ask-user steering response.
type CloudAskUserResponsePayload struct {
	PromptID    string `json:"promptId"`
	Answer      string `json:"answer"`
	WasFreeform bool   `json:"wasFreeform"`
	Dismissed   bool   `json:"dismissed,omitempty"`
}

// CloudPlanApprovalResponsePayload is the payload for a plan-approval steering response.
type CloudPlanApprovalResponsePayload struct {
	PromptID        string `json:"promptId"`
	Approved        bool   `json:"approved"`
	SelectedAction  string `json:"selectedAction,omitempty"`
	AutoApproveEdits bool  `json:"autoApproveEdits,omitempty"`
	Feedback        string `json:"feedback,omitempty"`
}

// CloudPermissionResponsePayload is the payload for a permission steering response.
type CloudPermissionResponsePayload struct {
	PromptID string `json:"promptId"`
	Approved bool   `json:"approved"`
	Scope    string `json:"scope"` // "once" or "session"
}

// CloudElicitationResponsePayload is the payload for an elicitation steering response.
type CloudElicitationResponsePayload struct {
	PromptID string         `json:"promptId"`
	Action   string         `json:"action"` // "accept", "decline", or "cancel"
	Content  map[string]any `json:"content,omitempty"`
}

// CloudModeSwitchPayload is the payload for a mode-switch steering command.
type CloudModeSwitchPayload struct {
	Mode string `json:"mode"` // "interactive", "plan", or "autopilot"
}

// CloudSessionError is returned when a Mission Control API request fails.
type CloudSessionError struct {
	Message string
	Reason  CloudSessionFailureReason
	Status  int
}

func (e *CloudSessionError) Error() string {
	return e.Message
}

// CloudSessionEventHandler is a callback invoked for every cloud session event.
type CloudSessionEventHandler func(CloudSessionEvent)

// CloudSessionOptions configures the creation of a new cloud session via
// [Client.CreateCloudSession].
type CloudSessionOptions struct {
	// Owner is the billing/authorization owner for repo-less cloud sandboxes.
	// Required when Repository is omitted.
	Owner string

	// Repository provides the repository context for the cloud sandbox.
	Repository *CloudRepository

	// MissionControlBaseURL overrides the Mission Control API base URL.
	MissionControlBaseURL string

	// CopilotAPIBaseURL overrides the Copilot API base URL used to derive
	// the Mission Control URL when MissionControlBaseURL is empty.
	CopilotAPIBaseURL string

	// FrontendBaseURL overrides the GitHub frontend base URL (default: https://github.com).
	FrontendBaseURL string

	// AuthToken overrides the authentication token sent to Mission Control.
	AuthToken string

	// IntegrationID overrides the Copilot-Integration-Id header (default: "copilot-cli").
	IntegrationID string

	// PollIntervalMs is the interval between event polls in milliseconds (default: 5000).
	PollIntervalMs int

	// InitialEventTimeoutMs is how long to wait for the first event in milliseconds (default: 10000).
	InitialEventTimeoutMs *int

	// InitialEventPollIntervalMs is the poll interval during the initial wait in milliseconds (default: 500).
	InitialEventPollIntervalMs int

	// OnProgress is called to report progress during session creation.
	OnProgress func(CloudProgressEvent)

	// OnCloudTaskCreated is called after the Mission Control task is created.
	OnCloudTaskCreated func(MissionControlTask)

	// OnEventPollError is called when an event poll fails.
	OnEventPollError func(error)
}

// CloudConnectOptions configures connecting to an existing cloud session via
// [Client.ConnectCloudSession].
type CloudConnectOptions struct {
	// Owner is the billing/authorization owner.
	Owner string

	// Repository provides optional repository context.
	Repository *CloudRepository

	// MissionControlBaseURL overrides the Mission Control API base URL.
	MissionControlBaseURL string

	// CopilotAPIBaseURL overrides the Copilot API base URL.
	CopilotAPIBaseURL string

	// FrontendBaseURL overrides the GitHub frontend base URL.
	FrontendBaseURL string

	// AuthToken overrides the authentication token.
	AuthToken string

	// IntegrationID overrides the Copilot-Integration-Id header.
	IntegrationID string

	// PollIntervalMs is the interval between event polls in milliseconds (default: 5000).
	PollIntervalMs int

	// InitialEventTimeoutMs is how long to wait for the first event in milliseconds (default: 10000).
	InitialEventTimeoutMs *int

	// InitialEventPollIntervalMs is the poll interval during the initial wait in milliseconds (default: 500).
	InitialEventPollIntervalMs int

	// OnProgress is called to report progress during connection.
	OnProgress func(CloudProgressEvent)

	// OnEventPollError is called when an event poll fails.
	OnEventPollError func(error)
}
