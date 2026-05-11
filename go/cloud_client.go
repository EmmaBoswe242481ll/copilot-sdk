package copilot

import (
	"errors"
	"os"
	"strings"
	"time"
)

// CreateCloudSession creates a sandbox-backed cloud session through Mission
// Control and attaches to it as a remote-control client.
//
// This does not create a local runtime session. The agent runs inside the
// provisioned cloud sandbox; this SDK instance polls Mission Control for
// events and sends user actions through the task steer API.
//
// Either options.Repository or options.Owner must be provided; when Repository
// is omitted, Owner is required for billing and authorization.
//
// Example:
//
//	session, err := client.CreateCloudSession(&copilot.CloudSessionOptions{
//	    Repository: &copilot.CloudRepository{Owner: "github", Name: "copilot-sdk"},
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer session.Disconnect()
//
//	session.On(func(event copilot.CloudSessionEvent) {
//	    fmt.Println(event.Type)
//	})
//	session.Send(copilot.MessageOptions{Prompt: "Hello from the cloud!"})
func (c *Client) CreateCloudSession(options *CloudSessionOptions) (*CloudSession, error) {
	if options == nil {
		options = &CloudSessionOptions{}
	}
	startedAt := time.Now()

	mcClient := c.buildMissionControlClient(
		options.MissionControlBaseURL,
		options.CopilotAPIBaseURL,
		options.FrontendBaseURL,
		options.AuthToken,
		options.IntegrationID,
	)

	owner := strings.TrimSpace(options.Owner)
	repo := options.Repository

	if repo == nil && owner == "" {
		return nil, errors.New("CloudSessionOptions.Owner is required when Repository is omitted")
	}

	if options.OnProgress != nil {
		options.OnProgress(CloudProgressEvent{Phase: CloudProgressCreatingTask, ElapsedMs: 0})
		options.OnProgress(CloudProgressEvent{
			Phase:     CloudProgressProvisioningSandbox,
			ElapsedMs: time.Since(startedAt).Milliseconds(),
		})
	}

	var taskRepo *CloudRepository
	if repo != nil {
		taskRepo = &CloudRepository{Owner: repo.Owner, Name: repo.Name}
	}
	task, err := mcClient.createCloudTask(owner, taskRepo)
	if err != nil {
		return nil, err
	}
	if options.OnCloudTaskCreated != nil {
		options.OnCloudTaskCreated(*task)
	}

	if options.OnProgress != nil {
		options.OnProgress(CloudProgressEvent{
			Phase:     CloudProgressWaitingForSession,
			ElapsedMs: time.Since(startedAt).Milliseconds(),
			TaskID:    task.ID,
		})
	}

	session := newCloudSession(cloudSessionConfig{
		client:                     mcClient,
		metadata:                   c.buildCloudSessionMetadata(task, mcClient, repo, owner),
		pollIntervalMs:             options.PollIntervalMs,
		initialEventTimeoutMs:      options.InitialEventTimeoutMs,
		initialEventPollIntervalMs: options.InitialEventPollIntervalMs,
		onEventPollError:           options.OnEventPollError,
	})

	if err := session.Connect(); err != nil {
		return nil, err
	}

	if options.OnProgress != nil {
		options.OnProgress(CloudProgressEvent{
			Phase:     CloudProgressConnected,
			ElapsedMs: time.Since(startedAt).Milliseconds(),
			TaskID:    task.ID,
		})
	}

	return session, nil
}

// ConnectCloudSession attaches to an existing Mission Control cloud task as a
// remote-control client.
//
// The taskOrSessionID is treated as a Mission Control task ID. If Mission
// Control returns task metadata, it is used to populate the session metadata;
// otherwise the SDK still attaches by polling task events for the provided ID.
//
// Example:
//
//	session, err := client.ConnectCloudSession("task-1", nil)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer session.Disconnect()
func (c *Client) ConnectCloudSession(taskOrSessionID string, options *CloudConnectOptions) (*CloudSession, error) {
	if options == nil {
		options = &CloudConnectOptions{}
	}
	startedAt := time.Now()

	mcClient := c.buildMissionControlClient(
		options.MissionControlBaseURL,
		options.CopilotAPIBaseURL,
		options.FrontendBaseURL,
		options.AuthToken,
		options.IntegrationID,
	)

	if options.OnProgress != nil {
		options.OnProgress(CloudProgressEvent{
			Phase:     CloudProgressWaitingForSession,
			ElapsedMs: 0,
			TaskID:    taskOrSessionID,
		})
	}

	owner := strings.TrimSpace(options.Owner)
	task, err := mcClient.getTask(taskOrSessionID)
	if err != nil {
		return nil, err
	}

	var metadata CloudSessionMetadata
	if task != nil {
		metadata = c.buildCloudSessionMetadata(task, mcClient, options.Repository, owner)
	} else {
		metadata = c.buildFallbackCloudSessionMetadata(taskOrSessionID, mcClient, options.Repository, owner)
	}

	session := newCloudSession(cloudSessionConfig{
		client:                     mcClient,
		metadata:                   metadata,
		pollIntervalMs:             options.PollIntervalMs,
		initialEventTimeoutMs:      options.InitialEventTimeoutMs,
		initialEventPollIntervalMs: options.InitialEventPollIntervalMs,
		onEventPollError:           options.OnEventPollError,
	})

	if err := session.Connect(); err != nil {
		return nil, err
	}

	if options.OnProgress != nil {
		options.OnProgress(CloudProgressEvent{
			Phase:     CloudProgressConnected,
			ElapsedMs: time.Since(startedAt).Milliseconds(),
			TaskID:    metadata.TaskID,
		})
	}

	return session, nil
}

// ── helpers ─────────────────────────────────────────────────────────────

func (c *Client) buildMissionControlClient(
	mcBaseURL, copilotAPIBaseURL, frontendBaseURL, authToken, integrationID string,
) *missionControlClient {
	copilotAPI := firstNonEmpty(
		copilotAPIBaseURL,
		os.Getenv("COPILOT_API_BASE_URL"),
		os.Getenv("COPILOT_API_URL"),
		"https://api.githubcopilot.com",
	)
	copilotAPI = strings.TrimRight(copilotAPI, "/")

	baseURL := firstNonEmpty(
		mcBaseURL,
		os.Getenv("COPILOT_MC_BASE_URL"),
		copilotAPI+"/agents",
	)

	token := firstNonEmpty(
		strings.TrimSpace(authToken),
		strings.TrimSpace(os.Getenv("COPILOT_MC_ACCESS_TOKEN")),
		c.options.GitHubToken,
	)

	frontend := firstNonEmpty(
		frontendBaseURL,
		os.Getenv("COPILOT_MC_FRONTEND_URL"),
		"https://github.com",
	)

	return newMissionControlClient(missionControlClientConfig{
		BaseURL:         baseURL,
		AuthToken:       token,
		IntegrationID:   integrationID,
		FrontendBaseURL: frontend,
	})
}

func (c *Client) buildCloudSessionMetadata(
	task *MissionControlTask,
	mcClient *missionControlClient,
	repository *CloudRepository,
	owner string,
) CloudSessionMetadata {
	var mcSessionID string
	if len(task.Sessions) > 0 {
		mcSessionID = task.Sessions[len(task.Sessions)-1].ID
	}

	createdAt, _ := time.Parse(time.RFC3339, task.CreatedAt)
	updatedAt, _ := time.Parse(time.RFC3339, task.UpdatedAt)

	return CloudSessionMetadata{
		TaskID:                  task.ID,
		MissionControlSessionID: mcSessionID,
		FrontendURL:             mcClient.getFrontendURL(task.ID),
		Owner:                   owner,
		Repository:              repository,
		CreatedAt:               createdAt,
		UpdatedAt:               updatedAt,
		State:                   task.State,
		Status:                  task.Status,
	}
}

func (c *Client) buildFallbackCloudSessionMetadata(
	taskID string,
	mcClient *missionControlClient,
	repository *CloudRepository,
	owner string,
) CloudSessionMetadata {
	now := time.Now()
	return CloudSessionMetadata{
		TaskID:     taskID,
		FrontendURL: mcClient.getFrontendURL(taskID),
		Owner:      owner,
		Repository: repository,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
