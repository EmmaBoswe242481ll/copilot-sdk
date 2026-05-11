package copilot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CloudSandboxAgentSlug is the agent slug sent when creating cloud sandbox tasks.
const CloudSandboxAgentSlug = "copilot-developer-sandbox"

const (
	defaultRequestTimeoutMs          = 10_000
	defaultCreateCloudTaskTimeoutMs  = 10 * 60 * 1000
)

// missionControlClientConfig holds the options for constructing a missionControlClient.
type missionControlClientConfig struct {
	BaseURL               string
	AuthToken             string
	IntegrationID         string
	FrontendBaseURL       string
	RequestTimeoutMs      int
	CreateCloudTaskTimeoutMs int
}

// missionControlClient talks to the Mission Control HTTP API.
type missionControlClient struct {
	baseURL               string
	authToken             string
	integrationID         string
	frontendBaseURL       string
	requestTimeout        time.Duration
	createCloudTaskTimeout time.Duration
	httpClient            *http.Client
}

func newMissionControlClient(cfg missionControlClientConfig) *missionControlClient {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	authToken := strings.TrimSpace(cfg.AuthToken)
	integrationID := cfg.IntegrationID
	if integrationID == "" {
		integrationID = "copilot-cli"
	}
	frontendBaseURL := strings.TrimRight(cfg.FrontendBaseURL, "/")

	reqTimeout := time.Duration(cfg.RequestTimeoutMs) * time.Millisecond
	if reqTimeout <= 0 {
		reqTimeout = time.Duration(defaultRequestTimeoutMs) * time.Millisecond
	}
	createTimeout := time.Duration(cfg.CreateCloudTaskTimeoutMs) * time.Millisecond
	if createTimeout <= 0 {
		createTimeout = time.Duration(defaultCreateCloudTaskTimeoutMs) * time.Millisecond
	}

	return &missionControlClient{
		baseURL:                baseURL,
		authToken:              authToken,
		integrationID:          integrationID,
		frontendBaseURL:        frontendBaseURL,
		requestTimeout:         reqTimeout,
		createCloudTaskTimeout: createTimeout,
		httpClient:             &http.Client{},
	}
}

func (mc *missionControlClient) createCloudTask(owner string, repo *CloudRepository) (*MissionControlTask, error) {
	body := make(map[string]any)
	if owner != "" {
		body["owner"] = owner
	}
	if repo != nil {
		body["repositories"] = []map[string]string{
			{"owner": repo.Owner, "name": repo.Name},
		}
	}

	var task MissionControlTask
	if err := mc.requestJSON(
		http.MethodPost,
		mc.baseURL+"/tasks",
		body,
		mc.createCloudTaskTimeout,
		map[string]string{"X-Copilot-Agent-Slug": CloudSandboxAgentSlug},
		&task,
	); err != nil {
		return nil, err
	}
	return &task, nil
}

func (mc *missionControlClient) listTaskEvents(taskID string) ([]CloudSessionEvent, error) {
	var wrapper struct {
		Events []CloudSessionEvent `json:"events"`
	}
	if err := mc.requestJSON(
		http.MethodGet,
		mc.baseURL+"/tasks/"+url.PathEscape(taskID)+"/events",
		nil,
		mc.requestTimeout,
		nil,
		&wrapper,
	); err != nil {
		return nil, err
	}
	// Filter events that have valid id, timestamp, and type fields.
	valid := make([]CloudSessionEvent, 0, len(wrapper.Events))
	for _, e := range wrapper.Events {
		if e.ID != "" && e.Timestamp != "" && e.Type != "" {
			valid = append(valid, e)
		}
	}
	return valid, nil
}

// steerRequest is the JSON body for the steer API.
type steerRequest struct {
	Type    MissionControlCommandType `json:"type"`
	Content string                    `json:"content,omitempty"`
}

func (mc *missionControlClient) steerTask(taskID string, cmdType MissionControlCommandType, content string) error {
	body := steerRequest{Type: cmdType}
	if content != "" {
		body.Content = content
	}
	return mc.requestOKOnly(
		http.MethodPost,
		mc.baseURL+"/tasks/"+url.PathEscape(taskID)+"/steer",
		body,
		mc.requestTimeout,
		nil,
	)
}

func (mc *missionControlClient) getTask(taskID string) (*MissionControlTask, error) {
	var task MissionControlTask
	if err := mc.requestJSON(
		http.MethodGet,
		mc.baseURL+"/tasks/"+url.PathEscape(taskID),
		nil,
		mc.requestTimeout,
		nil,
		&task,
	); err != nil {
		var csErr *CloudSessionError
		if ok := errorAs(err, &csErr); ok && csErr.Status == 404 {
			return nil, nil
		}
		return nil, err
	}
	return &task, nil
}

func (mc *missionControlClient) getFrontendURL(taskID string) string {
	return mc.frontendBaseURL + "/copilot/tasks/" + url.PathEscape(taskID)
}

// ── HTTP helpers ────────────────────────────────────────────────────────

func (mc *missionControlClient) headers(extra map[string]string) map[string]string {
	h := map[string]string{
		"Content-Type":           "application/json",
		"Copilot-Integration-Id": mc.integrationID,
	}
	if mc.authToken != "" {
		h["Authorization"] = "Bearer " + mc.authToken
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func (mc *missionControlClient) requestJSON(method, reqURL string, reqBody any, timeout time.Duration, extra map[string]string, out any) error {
	resp, err := mc.doRequest(method, reqURL, reqBody, timeout, extra)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return &CloudSessionError{Message: fmt.Sprintf("failed to read response body: %s", err), Reason: CloudFailureServer}
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &CloudSessionError{
			Message: fmt.Sprintf("Mission Control returned invalid JSON: %s", err),
			Reason:  CloudFailureServer,
		}
	}
	return nil
}

func (mc *missionControlClient) requestOKOnly(method, reqURL string, reqBody any, timeout time.Duration, extra map[string]string) error {
	resp, err := mc.doRequest(method, reqURL, reqBody, timeout, extra)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (mc *missionControlClient) doRequest(method, reqURL string, reqBody any, timeout time.Duration, extra map[string]string) (*http.Response, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, &CloudSessionError{Message: fmt.Sprintf("failed to marshal request: %s", err), Reason: CloudFailureServer}
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, reqURL, bodyReader)
	if err != nil {
		return nil, &CloudSessionError{Message: fmt.Sprintf("failed to create request: %s", err), Reason: CloudFailureNetwork}
	}
	for k, v := range mc.headers(extra) {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		if isTimeoutError(err) {
			return nil, &CloudSessionError{Message: "Mission Control request timed out", Reason: CloudFailureTimeout}
		}
		return nil, &CloudSessionError{
			Message: fmt.Sprintf("Mission Control request failed: %s", err),
			Reason:  CloudFailureNetwork,
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		msg := extractMissionControlMessage(body)
		if msg == "" {
			msg = fmt.Sprintf("Mission Control request failed with HTTP %d", resp.StatusCode)
		}
		return nil, &CloudSessionError{
			Message: msg,
			Reason:  reasonForStatus(resp.StatusCode),
			Status:  resp.StatusCode,
		}
	}

	return resp, nil
}

func reasonForStatus(status int) CloudSessionFailureReason {
	if status == 403 {
		return CloudFailurePolicyBlocked
	}
	if status == 400 || status == 422 {
		return CloudFailureValidation
	}
	return CloudFailureServer
}

func extractMissionControlMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parsed struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		return parsed.Message
	}
	return string(body)
}

func isTimeoutError(err error) bool {
	// net/http wraps timeout errors with a *url.Error whose Timeout() returns true.
	type timeouter interface{ Timeout() bool }
	if te, ok := err.(timeouter); ok {
		return te.Timeout()
	}
	return false
}

// errorAs is a thin wrapper around errors.As to support type-parameterized targets.
func errorAs(err error, target any) bool {
	type asIface interface {
		As(any) bool
	}
	switch t := target.(type) {
	case **CloudSessionError:
		for err != nil {
			if csErr, ok := err.(*CloudSessionError); ok {
				*t = csErr
				return true
			}
			u, ok := err.(interface{ Unwrap() error })
			if !ok {
				return false
			}
			err = u.Unwrap()
		}
		return false
	default:
		if a, ok := err.(asIface); ok {
			return a.As(target)
		}
		return false
	}
}
