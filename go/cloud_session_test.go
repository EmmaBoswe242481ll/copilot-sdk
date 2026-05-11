package copilot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ── helpers ─────────────────────────────────────────────────────────────

func newTestMCServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

var testTask = MissionControlTask{
	ID:           "task-1",
	Name:         "Cloud task",
	State:        "running",
	Status:       "ready",
	CreatorID:    1,
	OwnerID:      2,
	RepoID:       Int(3),
	SessionCount: 1,
	CreatedAt:    "2026-05-11T10:00:00.000Z",
	UpdatedAt:    "2026-05-11T10:01:00.000Z",
	Sessions: []MissionControlTaskSession{
		{
			ID:        "mc-session-1",
			TaskID:    "task-1",
			State:     "running",
			CreatedAt: "2026-05-11T10:00:30.000Z",
			UpdatedAt: "2026-05-11T10:00:30.000Z",
			OwnerID:   2,
			RepoID:    Int(3),
		},
	},
}

var requestedEvent = CloudSessionEvent{
	ID:        "event-1",
	Timestamp: "2026-05-11T10:00:00.000Z",
	Type:      "session.requested",
}

var idleEvent = CloudSessionEvent{
	ID:        "event-2",
	Timestamp: "2026-05-11T10:00:01.000Z",
	Type:      "session.idle",
	Data:      json.RawMessage(`{}`),
}

// ── tests ───────────────────────────────────────────────────────────────

func TestCreateCloudSession_WithRepository(t *testing.T) {
	var capturedRequests []capturedRequest
	var mu sync.Mutex

	call := 0
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedRequests = append(capturedRequests, capture(r))
		mu.Unlock()

		switch call {
		case 0: // createCloudTask
			w.Header().Set("Content-Type", "application/json")
			w.Write(jsonBytes(testTask))
		case 1: // listTaskEvents
			w.Header().Set("Content-Type", "application/json")
			w.Write(jsonBytes(map[string]any{"events": []CloudSessionEvent{requestedEvent}}))
		default:
			w.WriteHeader(500)
		}
		call++
	})
	defer server.Close()

	progress := []CloudProgressPhase{}

	client := NewClient(&ClientOptions{AutoStart: Bool(false), GitHubToken: "token-1"})
	session, err := client.CreateCloudSession(&CloudSessionOptions{
		Repository:            &CloudRepository{Owner: "github", Name: "copilot-sdk", Branch: "main"},
		MissionControlBaseURL: server.URL,
		FrontendBaseURL:       "https://github.test",
		InitialEventTimeoutMs: Int(0),
		OnProgress: func(e CloudProgressEvent) {
			progress = append(progress, e.Phase)
		},
	})
	if err != nil {
		t.Fatalf("CreateCloudSession failed: %v", err)
	}
	defer session.Disconnect()

	// Verify metadata
	if session.Metadata.TaskID != "task-1" {
		t.Errorf("expected taskId task-1, got %s", session.Metadata.TaskID)
	}
	if session.Metadata.MissionControlSessionID != "mc-session-1" {
		t.Errorf("expected MC session mc-session-1, got %s", session.Metadata.MissionControlSessionID)
	}
	if session.Metadata.FrontendURL != "https://github.test/copilot/tasks/task-1" {
		t.Errorf("unexpected frontendUrl: %s", session.Metadata.FrontendURL)
	}
	if session.Metadata.Repository == nil || session.Metadata.Repository.Owner != "github" {
		t.Error("expected repository metadata to be set")
	}
	if session.Metadata.State != "running" || session.Metadata.Status != "ready" {
		t.Errorf("unexpected state/status: %s/%s", session.Metadata.State, session.Metadata.Status)
	}

	// Verify events
	msgs := session.GetMessages()
	if len(msgs) != 1 || msgs[0].ID != "event-1" {
		t.Errorf("expected 1 event, got %d", len(msgs))
	}

	// Verify progress
	expectedProgress := []CloudProgressPhase{
		CloudProgressCreatingTask,
		CloudProgressProvisioningSandbox,
		CloudProgressWaitingForSession,
		CloudProgressConnected,
	}
	if len(progress) != len(expectedProgress) {
		t.Fatalf("expected %d progress events, got %d: %v", len(expectedProgress), len(progress), progress)
	}
	for i, p := range expectedProgress {
		if progress[i] != p {
			t.Errorf("progress[%d]: expected %s, got %s", i, p, progress[i])
		}
	}

	// Verify create task request
	mu.Lock()
	createReq := capturedRequests[0]
	mu.Unlock()
	if createReq.method != "POST" {
		t.Errorf("expected POST, got %s", createReq.method)
	}
	if createReq.headers["X-Copilot-Agent-Slug"] != CloudSandboxAgentSlug {
		t.Errorf("missing agent slug header")
	}
	if createReq.headers["Authorization"] != "Bearer token-1" {
		t.Errorf("unexpected auth header: %s", createReq.headers["Authorization"])
	}

	var body map[string]any
	json.Unmarshal(createReq.body, &body)
	repos, ok := body["repositories"]
	if !ok {
		t.Fatal("expected repositories in request body")
	}
	repoList, ok := repos.([]any)
	if !ok || len(repoList) != 1 {
		t.Fatal("expected 1 repository in request body")
	}
	repoMap := repoList[0].(map[string]any)
	if repoMap["owner"] != "github" || repoMap["name"] != "copilot-sdk" {
		t.Errorf("unexpected repo: %v", repoMap)
	}
}

func TestCreateCloudSession_RepoLessWithOwner(t *testing.T) {
	call := 0
	var capturedBody []byte
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 0:
			b := make([]byte, r.ContentLength)
			r.Body.Read(b)
			capturedBody = b
			w.Write(jsonBytes(testTask))
		case 1:
			w.Write(jsonBytes(map[string]any{"events": []any{}}))
		}
		call++
	})
	defer server.Close()

	client := NewClient(&ClientOptions{AutoStart: Bool(false)})
	session, err := client.CreateCloudSession(&CloudSessionOptions{
		Owner:                 "github",
		MissionControlBaseURL: server.URL,
		InitialEventTimeoutMs: Int(0),
	})
	if err != nil {
		t.Fatalf("CreateCloudSession failed: %v", err)
	}
	defer session.Disconnect()

	if session.Metadata.Owner != "github" {
		t.Errorf("expected owner github, got %s", session.Metadata.Owner)
	}

	var body map[string]any
	json.Unmarshal(capturedBody, &body)
	if body["owner"] != "github" {
		t.Errorf("expected owner in body, got %v", body)
	}
	if _, hasRepos := body["repositories"]; hasRepos {
		t.Error("repo-less request should not have repositories key")
	}
}

func TestCreateCloudSession_RequiresOwnerWhenRepoOmitted(t *testing.T) {
	client := NewClient(&ClientOptions{AutoStart: Bool(false)})

	_, err := client.CreateCloudSession(&CloudSessionOptions{
		InitialEventTimeoutMs: Int(0),
	})
	if err == nil {
		t.Fatal("expected error when both owner and repository are omitted")
	}
	if err.Error() != "CloudSessionOptions.Owner is required when Repository is omitted" {
		t.Errorf("unexpected error message: %s", err)
	}
}

func TestConnectCloudSession_SteerAPI(t *testing.T) {
	call := 0
	var steerBody []byte
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 0: // getTask → 404
			w.WriteHeader(404)
			w.Write([]byte(`"not found"`))
		case 1: // listTaskEvents
			w.Write(jsonBytes(map[string]any{"events": []any{}}))
		case 2: // steer
			b := make([]byte, r.ContentLength)
			r.Body.Read(b)
			steerBody = b
			w.WriteHeader(202)
		}
		call++
	})
	defer server.Close()

	client := NewClient(&ClientOptions{AutoStart: Bool(false)})
	session, err := client.ConnectCloudSession("task-1", &CloudConnectOptions{
		MissionControlBaseURL: server.URL,
		InitialEventTimeoutMs: Int(0),
	})
	if err != nil {
		t.Fatalf("ConnectCloudSession failed: %v", err)
	}
	defer session.Disconnect()

	if err := session.Send(MessageOptions{Prompt: "hello cloud"}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	var steer steerRequest
	if err := json.Unmarshal(steerBody, &steer); err != nil {
		t.Fatalf("failed to parse steer body: %v", err)
	}
	if steer.Type != CommandUserMessage {
		t.Errorf("expected user_message, got %s", steer.Type)
	}
	if steer.Content != "hello cloud" {
		t.Errorf("expected content 'hello cloud', got %s", steer.Content)
	}
}

func TestCloudSession_SortsAndDeduplicatesEvents(t *testing.T) {
	polledEvent := CloudSessionEvent{
		ID:        "event-3",
		Timestamp: "2026-05-11T10:00:02.000Z",
		Type:      "session.idle",
		Data:      json.RawMessage(`{}`),
	}

	call := 0
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 0: // getTask
			w.Write(jsonBytes(testTask))
		case 1: // listTaskEvents (initial) - note reversed order
			w.Write(jsonBytes(map[string]any{"events": []CloudSessionEvent{idleEvent, requestedEvent}}))
		default: // listTaskEvents (poll) - includes old + new
			w.Write(jsonBytes(map[string]any{"events": []CloudSessionEvent{idleEvent, requestedEvent, polledEvent}}))
		}
		call++
	})
	defer server.Close()

	client := NewClient(&ClientOptions{AutoStart: Bool(false)})
	session, err := client.ConnectCloudSession("task-1", &CloudConnectOptions{
		MissionControlBaseURL: server.URL,
		InitialEventTimeoutMs: Int(0),
		PollIntervalMs:        10,
	})
	if err != nil {
		t.Fatalf("ConnectCloudSession failed: %v", err)
	}
	defer session.Disconnect()

	// Initial events should be sorted chronologically
	msgs := session.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 initial events, got %d", len(msgs))
	}
	if msgs[0].ID != "event-1" || msgs[1].ID != "event-2" {
		t.Errorf("initial events not sorted: %s, %s", msgs[0].ID, msgs[1].ID)
	}

	// Wait for a poll cycle
	seenCh := make(chan string, 10)
	session.On(func(event CloudSessionEvent) {
		seenCh <- event.ID
	})

	// Wait for the polled event
	timeout := time.After(2 * time.Second)
	select {
	case id := <-seenCh:
		if id != "event-3" {
			t.Errorf("expected event-3, got %s", id)
		}
	case <-timeout:
		t.Fatal("timed out waiting for polled event")
	}

	// Total events should be 3 with no duplicates
	allMsgs := session.GetMessages()
	if len(allMsgs) != 3 {
		t.Fatalf("expected 3 total events, got %d", len(allMsgs))
	}
	expectedIDs := []string{"event-1", "event-2", "event-3"}
	for i, expected := range expectedIDs {
		if allMsgs[i].ID != expected {
			t.Errorf("event[%d]: expected %s, got %s", i, expected, allMsgs[i].ID)
		}
	}
}

func TestCloudSession_MissionControlErrorResponse(t *testing.T) {
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write(jsonBytes(map[string]string{"message": "blocked"}))
	})
	defer server.Close()

	client := NewClient(&ClientOptions{AutoStart: Bool(false)})
	_, err := client.CreateCloudSession(&CloudSessionOptions{
		Repository:            &CloudRepository{Owner: "github", Name: "copilot-sdk"},
		MissionControlBaseURL: server.URL,
		InitialEventTimeoutMs: Int(0),
	})
	if err == nil {
		t.Fatal("expected error")
	}

	csErr, ok := err.(*CloudSessionError)
	if !ok {
		t.Fatalf("expected CloudSessionError, got %T: %v", err, err)
	}
	if csErr.Message != "blocked" {
		t.Errorf("expected message 'blocked', got %s", csErr.Message)
	}
	if csErr.Reason != CloudFailurePolicyBlocked {
		t.Errorf("expected reason policy_blocked, got %s", csErr.Reason)
	}
	if csErr.Status != 403 {
		t.Errorf("expected status 403, got %d", csErr.Status)
	}
}

func TestCloudSession_DisconnectPreventsSteer(t *testing.T) {
	call := 0
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 0:
			w.WriteHeader(404)
			w.Write([]byte(`"not found"`))
		case 1:
			w.Write(jsonBytes(map[string]any{"events": []any{}}))
		}
		call++
	})
	defer server.Close()

	client := NewClient(&ClientOptions{AutoStart: Bool(false)})
	session, err := client.ConnectCloudSession("task-1", &CloudConnectOptions{
		MissionControlBaseURL: server.URL,
		InitialEventTimeoutMs: Int(0),
	})
	if err != nil {
		t.Fatalf("ConnectCloudSession failed: %v", err)
	}

	session.Disconnect()

	if err := session.Send(MessageOptions{Prompt: "should fail"}); err == nil {
		t.Fatal("expected error after disconnect")
	}
}

func TestCloudSession_RemoteSteerableChanged(t *testing.T) {
	steerableEvent := CloudSessionEvent{
		ID:        "event-steerable",
		Timestamp: "2026-05-11T10:00:03.000Z",
		Type:      "session.remote_steerable_changed",
		Data:      json.RawMessage(`{"remoteSteerable": false}`),
	}

	call := 0
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 0:
			w.WriteHeader(404)
			w.Write([]byte(`"not found"`))
		case 1:
			w.Write(jsonBytes(map[string]any{"events": []CloudSessionEvent{requestedEvent, steerableEvent}}))
		}
		call++
	})
	defer server.Close()

	client := NewClient(&ClientOptions{AutoStart: Bool(false)})
	session, err := client.ConnectCloudSession("task-1", &CloudConnectOptions{
		MissionControlBaseURL: server.URL,
		InitialEventTimeoutMs: Int(0),
	})
	if err != nil {
		t.Fatalf("ConnectCloudSession failed: %v", err)
	}
	defer session.Disconnect()

	err = session.Send(MessageOptions{Prompt: "should fail"})
	if err == nil {
		t.Fatal("expected error when not steerable")
	}
	if err.Error() != "this session is read-only — remote steering is not enabled" {
		t.Errorf("unexpected error: %s", err)
	}
}

// ── capture helper ──────────────────────────────────────────────────────

type capturedRequest struct {
	method  string
	path    string
	headers map[string]string
	body    []byte
}

func capture(r *http.Request) capturedRequest {
	body := make([]byte, 0)
	if r.Body != nil {
		body = make([]byte, r.ContentLength)
		r.Body.Read(body)
	}
	headers := make(map[string]string)
	for k := range r.Header {
		headers[k] = r.Header.Get(k)
	}
	return capturedRequest{
		method:  r.Method,
		path:    r.URL.Path,
		headers: headers,
		body:    body,
	}
}

func TestCloudSession_OnEventHandler(t *testing.T) {
	call := 0
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 0:
			w.WriteHeader(404)
			w.Write([]byte(`"not found"`))
		case 1:
			w.Write(jsonBytes(map[string]any{"events": []CloudSessionEvent{requestedEvent, idleEvent}}))
		}
		call++
	})
	defer server.Close()

	client := NewClient(&ClientOptions{AutoStart: Bool(false)})
	session, err := client.ConnectCloudSession("task-1", &CloudConnectOptions{
		MissionControlBaseURL: server.URL,
		InitialEventTimeoutMs: Int(0),
	})
	if err != nil {
		t.Fatalf("ConnectCloudSession failed: %v", err)
	}
	defer session.Disconnect()

	// Handler registered after connect should not receive replayed initial events
	// but getMessages should still return them.
	var handlerEvents []string
	session.On(func(event CloudSessionEvent) {
		handlerEvents = append(handlerEvents, event.ID)
	})

	msgs := session.GetMessages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 initial events, got %d", len(msgs))
	}
	if msgs[0].ID != "event-1" || msgs[1].ID != "event-2" {
		t.Errorf("unexpected event order: %s, %s", msgs[0].ID, msgs[1].ID)
	}

	// No handler events since handler was registered after connect
	if len(handlerEvents) != 0 {
		t.Errorf("handler should not have received replay events, got %v", handlerEvents)
	}
}

func TestCloudSession_UnsubscribeHandler(t *testing.T) {
	polledEvent := CloudSessionEvent{
		ID:        "event-3",
		Timestamp: "2026-05-11T10:00:02.000Z",
		Type:      "session.idle",
		Data:      json.RawMessage(`{}`),
	}

	call := 0
	server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 0:
			w.WriteHeader(404)
			w.Write([]byte(`"not found"`))
		case 1:
			w.Write(jsonBytes(map[string]any{"events": []CloudSessionEvent{requestedEvent}}))
		default:
			w.Write(jsonBytes(map[string]any{"events": []CloudSessionEvent{requestedEvent, polledEvent}}))
		}
		call++
	})
	defer server.Close()

	client := NewClient(&ClientOptions{AutoStart: Bool(false)})
	session, err := client.ConnectCloudSession("task-1", &CloudConnectOptions{
		MissionControlBaseURL: server.URL,
		InitialEventTimeoutMs: Int(0),
		PollIntervalMs:        10,
	})
	if err != nil {
		t.Fatalf("ConnectCloudSession failed: %v", err)
	}
	defer session.Disconnect()

	callCount := 0
	var countMu sync.Mutex
	unsubscribe := session.On(func(event CloudSessionEvent) {
		countMu.Lock()
		callCount++
		countMu.Unlock()
	})

	// Immediately unsubscribe — the handler should never fire for polled events
	unsubscribe()

	time.Sleep(50 * time.Millisecond)

	countMu.Lock()
	count := callCount
	countMu.Unlock()
	if count != 0 {
		t.Errorf("handler was called %d times after unsubscribe", count)
	}
}

func TestMissionControlClient_ErrorExtraction(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		expectedReason CloudSessionFailureReason
		expectedMsg    string
	}{
		{
			name:           "403 with JSON message",
			status:         403,
			body:           `{"message": "policy blocked"}`,
			expectedReason: CloudFailurePolicyBlocked,
			expectedMsg:    "policy blocked",
		},
		{
			name:           "400 validation error",
			status:         400,
			body:           `{"message": "invalid request"}`,
			expectedReason: CloudFailureValidation,
			expectedMsg:    "invalid request",
		},
		{
			name:           "422 validation error",
			status:         422,
			body:           `{"message": "unprocessable"}`,
			expectedReason: CloudFailureValidation,
			expectedMsg:    "unprocessable",
		},
		{
			name:           "500 server error with no body",
			status:         500,
			body:           "",
			expectedReason: CloudFailureServer,
			expectedMsg:    fmt.Sprintf("Mission Control request failed with HTTP %d", 500),
		},
		{
			name:           "500 with plain text body",
			status:         500,
			body:           "internal error",
			expectedReason: CloudFailureServer,
			expectedMsg:    "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestMCServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			})
			defer server.Close()

			mc := newMissionControlClient(missionControlClientConfig{
				BaseURL:         server.URL,
				FrontendBaseURL: "https://github.test",
			})

			_, err := mc.createCloudTask("owner", nil)
			if err == nil {
				t.Fatal("expected error")
			}
			csErr, ok := err.(*CloudSessionError)
			if !ok {
				t.Fatalf("expected CloudSessionError, got %T", err)
			}
			if csErr.Reason != tt.expectedReason {
				t.Errorf("expected reason %s, got %s", tt.expectedReason, csErr.Reason)
			}
			if csErr.Message != tt.expectedMsg {
				t.Errorf("expected message %q, got %q", tt.expectedMsg, csErr.Message)
			}
			if csErr.Status != tt.status {
				t.Errorf("expected status %d, got %d", tt.status, csErr.Status)
			}
		})
	}
}
