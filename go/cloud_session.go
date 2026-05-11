package copilot

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	defaultPollIntervalMs            = 5_000
	defaultInitialEventTimeoutMs     = 10_000
	defaultInitialEventPollIntervalMs = 500
)

// cloudSessionConfig is the internal configuration for creating a CloudSession.
type cloudSessionConfig struct {
	client                     *missionControlClient
	metadata                   CloudSessionMetadata
	pollIntervalMs             int
	initialEventTimeoutMs      *int // nil = use default
	initialEventPollIntervalMs int
	onEventPollError           func(error)
}

// CloudSession represents a remote cloud sandbox session controlled through
// the Mission Control API. It polls for task events, dispatches them to
// registered handlers, and provides methods for steering the remote agent.
//
// A CloudSession does not run a local CLI process. All agent work happens in
// the cloud sandbox; this object is the remote-control client.
type CloudSession struct {
	client                    *missionControlClient
	pollInterval              time.Duration
	initialEventTimeout       time.Duration
	initialEventPollInterval  time.Duration
	onEventPollError          func(error)

	mu                        sync.Mutex
	handlers                  []cloudHandler
	nextHandlerID             uint64
	events                    []CloudSessionEvent
	seenEventIDs              map[string]bool
	seenIDsAtLastTimestamp    map[string]bool
	lastSeenTimestamp         string
	stopPolling               chan struct{}
	isPolling                 bool
	isDisconnected            bool
	remoteSteerable           bool

	// SessionID is the Mission Control session identifier.
	SessionID string

	// Metadata holds the full metadata for this cloud session.
	Metadata CloudSessionMetadata
}

type cloudHandler struct {
	id uint64
	fn CloudSessionEventHandler
}

func newCloudSession(cfg cloudSessionConfig) *CloudSession {
	pollMs := cfg.pollIntervalMs
	if pollMs <= 0 {
		pollMs = defaultPollIntervalMs
	}
	initTimeoutMs := defaultInitialEventTimeoutMs
	if cfg.initialEventTimeoutMs != nil {
		initTimeoutMs = *cfg.initialEventTimeoutMs
		if initTimeoutMs < 0 {
			initTimeoutMs = 0
		}
	}
	initPollMs := cfg.initialEventPollIntervalMs
	if initPollMs <= 0 {
		initPollMs = defaultInitialEventPollIntervalMs
	}

	sessionID := cfg.metadata.MissionControlSessionID
	if sessionID == "" {
		sessionID = cfg.metadata.TaskID
	}

	return &CloudSession{
		client:                   cfg.client,
		pollInterval:             time.Duration(pollMs) * time.Millisecond,
		initialEventTimeout:      time.Duration(initTimeoutMs) * time.Millisecond,
		initialEventPollInterval: time.Duration(initPollMs) * time.Millisecond,
		onEventPollError:         cfg.onEventPollError,
		handlers:                 make([]cloudHandler, 0),
		events:                   make([]CloudSessionEvent, 0),
		seenEventIDs:             make(map[string]bool),
		seenIDsAtLastTimestamp:   make(map[string]bool),
		remoteSteerable:          true,
		SessionID:                sessionID,
		Metadata:                 cfg.metadata,
	}
}

// Connect fetches initial events and starts background polling.
// It must be called before sending messages or subscribing to events.
func (cs *CloudSession) Connect() error {
	initial, err := cs.waitForInitialEvents()
	if err != nil {
		return err
	}
	cs.recordEvents(initial)
	cs.startEventPolling()
	return nil
}

// On registers a handler that is called for every cloud session event.
// Returns an unsubscribe function.
func (cs *CloudSession) On(handler CloudSessionEventHandler) func() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	id := cs.nextHandlerID
	cs.nextHandlerID++
	cs.handlers = append(cs.handlers, cloudHandler{id: id, fn: handler})

	return func() {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		for i, h := range cs.handlers {
			if h.id == id {
				cs.handlers = append(cs.handlers[:i], cs.handlers[i+1:]...)
				break
			}
		}
	}
}

// Send sends a user message to the cloud session through the steer API.
func (cs *CloudSession) Send(options MessageOptions) error {
	cs.mu.Lock()
	if cs.isDisconnected {
		cs.mu.Unlock()
		return errors.New("cloud session is disconnected")
	}
	cs.mu.Unlock()
	return cs.SubmitRemoteCommand(CommandUserMessage, options.Prompt)
}

// SendAndWait sends a user message and blocks until the session becomes idle
// or the timeout elapses. Returns the last assistant.message event received,
// or nil if none. The default timeout is 60 seconds.
func (cs *CloudSession) SendAndWait(options MessageOptions, timeout time.Duration) (*CloudSessionEvent, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	var lastAssistant *CloudSessionEvent
	var mu sync.Mutex
	idleCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	unsubscribe := cs.On(func(event CloudSessionEvent) {
		switch event.Type {
		case "assistant.message":
			mu.Lock()
			copied := event
			lastAssistant = &copied
			mu.Unlock()
		case "session.idle":
			select {
			case idleCh <- struct{}{}:
			default:
			}
		case "session.error":
			var msg string
			var data struct{ Message string }
			if err := json.Unmarshal(event.Data, &data); err == nil {
				msg = data.Message
			}
			select {
			case errCh <- fmt.Errorf("session error: %s", msg):
			default:
			}
		}
	})
	defer unsubscribe()

	if err := cs.Send(options); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-idleCh:
		mu.Lock()
		result := lastAssistant
		mu.Unlock()
		return result, nil
	case err := <-errCh:
		return nil, err
	case <-timer.C:
		return nil, fmt.Errorf("timeout after %s waiting for session.idle", timeout)
	}
}

// Abort aborts the currently running agent work.
func (cs *CloudSession) Abort() error {
	cs.mu.Lock()
	if cs.isDisconnected {
		cs.mu.Unlock()
		return errors.New("cloud session is disconnected")
	}
	cs.mu.Unlock()
	return cs.SubmitRemoteCommand(CommandAbort, "")
}

// SubmitRemoteCommand sends a steering command to the cloud session.
func (cs *CloudSession) SubmitRemoteCommand(cmdType MissionControlCommandType, content string) error {
	cs.mu.Lock()
	if cs.isDisconnected {
		cs.mu.Unlock()
		return errors.New("cloud session is disconnected")
	}
	if !cs.remoteSteerable {
		cs.mu.Unlock()
		return errors.New("this session is read-only — remote steering is not enabled")
	}
	cs.mu.Unlock()
	return cs.client.steerTask(cs.Metadata.TaskID, cmdType, content)
}

// RespondToPermission sends a permission response to the cloud session.
func (cs *CloudSession) RespondToPermission(payload CloudPermissionResponsePayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return cs.SubmitRemoteCommand(CommandPermissionResponse, string(data))
}

// RespondToAskUser sends an ask-user response to the cloud session.
func (cs *CloudSession) RespondToAskUser(payload CloudAskUserResponsePayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return cs.SubmitRemoteCommand(CommandAskUserResponse, string(data))
}

// RespondToElicitation sends an elicitation response to the cloud session.
func (cs *CloudSession) RespondToElicitation(payload CloudElicitationResponsePayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return cs.SubmitRemoteCommand(CommandElicitation, string(data))
}

// RespondToPlanApproval sends a plan-approval response to the cloud session.
func (cs *CloudSession) RespondToPlanApproval(payload CloudPlanApprovalResponsePayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return cs.SubmitRemoteCommand(CommandPlanApproval, string(data))
}

// SwitchMode sends a mode-switch command to the cloud session.
func (cs *CloudSession) SwitchMode(payload CloudModeSwitchPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return cs.SubmitRemoteCommand(CommandModeSwitch, string(data))
}

// GetMessages returns a copy of all events received so far, in chronological order.
func (cs *CloudSession) GetMessages() []CloudSessionEvent {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]CloudSessionEvent, len(cs.events))
	copy(out, cs.events)
	return out
}

// Disconnect stops event polling and clears all handlers. The session cannot
// be used after disconnecting.
func (cs *CloudSession) Disconnect() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.isDisconnected {
		return
	}
	cs.isDisconnected = true
	cs.stopEventPolling()
	cs.handlers = nil
}

// ── internal polling ────────────────────────────────────────────────────

func (cs *CloudSession) startEventPolling() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.stopPolling != nil || cs.isDisconnected {
		return
	}

	stop := make(chan struct{})
	cs.stopPolling = stop

	go func() {
		ticker := time.NewTicker(cs.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				cs.pollEvents()
			}
		}
	}()
}

// stopEventPolling must be called with cs.mu held.
func (cs *CloudSession) stopEventPolling() {
	if cs.stopPolling != nil {
		close(cs.stopPolling)
		cs.stopPolling = nil
	}
}

func (cs *CloudSession) waitForInitialEvents() ([]CloudSessionEvent, error) {
	deadline := time.Now().Add(cs.initialEventTimeout)
	for {
		events, err := cs.client.listTaskEvents(cs.Metadata.TaskID)
		if err != nil {
			return nil, err
		}
		if len(events) > 0 {
			return sortEventsChronologically(events), nil
		}
		if cs.initialEventTimeout <= 0 || time.Now().After(deadline) {
			return nil, nil
		}
		time.Sleep(cs.initialEventPollInterval)
	}
}

func (cs *CloudSession) pollEvents() {
	cs.mu.Lock()
	if cs.isPolling || cs.isDisconnected {
		cs.mu.Unlock()
		return
	}
	cs.isPolling = true
	cs.mu.Unlock()

	defer func() {
		cs.mu.Lock()
		cs.isPolling = false
		cs.mu.Unlock()
	}()

	events, err := cs.client.listTaskEvents(cs.Metadata.TaskID)
	if err != nil {
		if cs.onEventPollError != nil {
			cs.onEventPollError(err)
		}
		return
	}

	newEvents := cs.collectNewEvents(events)
	cs.recordEvents(newEvents)
}

func (cs *CloudSession) collectNewEvents(events []CloudSessionEvent) []CloudSessionEvent {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var newEvents []CloudSessionEvent
	for _, e := range events {
		if cs.seenEventIDs[e.ID] {
			continue
		}
		if cs.lastSeenTimestamp == "" {
			newEvents = append(newEvents, e)
			continue
		}
		cmp := compareTimestamps(e.Timestamp, cs.lastSeenTimestamp)
		if cmp > 0 {
			newEvents = append(newEvents, e)
		} else if cmp == 0 && !cs.seenIDsAtLastTimestamp[e.ID] {
			newEvents = append(newEvents, e)
		}
	}

	return sortEventsChronologically(newEvents)
}

func (cs *CloudSession) recordEvents(events []CloudSessionEvent) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for _, e := range sortEventsChronologically(events) {
		if cs.seenEventIDs[e.ID] {
			continue
		}
		cs.seenEventIDs[e.ID] = true
		cs.events = append(cs.events, e)
		cs.markEventTimestamp(e)
		cs.updateRemoteSteerable(e)
		cs.dispatchEvent(e)
	}
}

func (cs *CloudSession) markEventTimestamp(e CloudSessionEvent) {
	if cs.lastSeenTimestamp != e.Timestamp {
		cs.lastSeenTimestamp = e.Timestamp
		cs.seenIDsAtLastTimestamp = make(map[string]bool)
	}
	cs.seenIDsAtLastTimestamp[e.ID] = true
}

func (cs *CloudSession) updateRemoteSteerable(e CloudSessionEvent) {
	if e.Type == "session.remote_steerable_changed" {
		var data struct {
			RemoteSteerable bool `json:"remoteSteerable"`
		}
		if err := json.Unmarshal(e.Data, &data); err == nil {
			cs.remoteSteerable = data.RemoteSteerable
		}
	}
}

// dispatchEvent must be called with cs.mu held.
func (cs *CloudSession) dispatchEvent(e CloudSessionEvent) {
	// Copy handlers to allow safe iteration; callers may unsubscribe inside handlers.
	handlers := make([]cloudHandler, len(cs.handlers))
	copy(handlers, cs.handlers)

	for _, h := range handlers {
		func() {
			defer func() { recover() }() // keep one failing handler from breaking polling
			h.fn(e)
		}()
	}
}

func sortEventsChronologically(events []CloudSessionEvent) []CloudSessionEvent {
	sorted := make([]CloudSessionEvent, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		cmp := compareTimestamps(sorted[i].Timestamp, sorted[j].Timestamp)
		if cmp != 0 {
			return cmp < 0
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted
}

func compareTimestamps(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
