package observability

import (
	"context"
	"net/http"
)

type contextKey string

const requestStateContextKey contextKey = "observability_request_state"

// RequestState holds per-request observability state, including the optional wide event.
type RequestState struct {
	WideEvent *WideEvent
}

// WideEvent is a structured observability event that accumulates attributes during request processing.
type WideEvent struct {
	Name         string         `json:"name"`
	Category     string         `json:"category"`
	ResourceType string         `json:"resource_type"`
	Action       string         `json:"action"`
	Attributes   map[string]any `json:"attributes,omitempty"`
}

// NewWideEvent creates a WideEvent with the given classification fields.
func NewWideEvent(name, category, resourceType, action string) *WideEvent {
	return &WideEvent{
		Name:         name,
		Category:     category,
		ResourceType: resourceType,
		Action:       action,
		Attributes:   map[string]any{},
	}
}

// AddAttributes merges the provided key-value pairs into the event's attributes.
func (event *WideEvent) AddAttributes(attributes map[string]any) *WideEvent {
	if event == nil {
		return nil
	}
	if event.Attributes == nil {
		event.Attributes = map[string]any{}
	}
	for key, value := range attributes {
		event.Attributes[key] = value
	}
	return event
}

// RequestStateFromContext extracts the RequestState from the context, if present.
func RequestStateFromContext(ctx context.Context) (*RequestState, bool) {
	state, ok := ctx.Value(requestStateContextKey).(*RequestState)
	return state, ok && state != nil
}

// WideEventFromContext returns a clone of the WideEvent from the request context, if set.
func WideEventFromContext(ctx context.Context) (*WideEvent, bool) {
	state, ok := RequestStateFromContext(ctx)
	if !ok || state.WideEvent == nil {
		return nil, false
	}
	return state.WideEvent.Clone(), true
}

// SetWideEvent attaches a WideEvent to the request's observability state.
func SetWideEvent(request *http.Request, event *WideEvent) {
	state, ok := RequestStateFromContext(request.Context())
	if !ok || event == nil {
		return
	}
	state.WideEvent = event.Clone()
}

// AddWideEventAttributes merges attributes into the request's current WideEvent, creating one if needed.
func AddWideEventAttributes(request *http.Request, attributes map[string]any) {
	state, ok := RequestStateFromContext(request.Context())
	if !ok {
		return
	}
	if state.WideEvent == nil {
		state.WideEvent = NewWideEvent("", "", "", "")
	}
	state.WideEvent.AddAttributes(attributes)
}

func withRequestState(request *http.Request) *http.Request {
	if _, ok := RequestStateFromContext(request.Context()); ok {
		return request
	}
	state := &RequestState{}
	ctx := context.WithValue(request.Context(), requestStateContextKey, state)
	return request.WithContext(ctx)
}

// Clone returns a deep copy of the WideEvent.
func (event *WideEvent) Clone() *WideEvent {
	if event == nil {
		return nil
	}
	clone := &WideEvent{
		Name:         event.Name,
		Category:     event.Category,
		ResourceType: event.ResourceType,
		Action:       event.Action,
	}
	if len(event.Attributes) > 0 {
		clone.Attributes = make(map[string]any, len(event.Attributes))
		for key, value := range event.Attributes {
			clone.Attributes[key] = value
		}
	}
	return clone
}
