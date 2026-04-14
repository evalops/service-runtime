package agenthook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ToolUseEvent is the generic PreToolUse envelope shared by Codex and Claude Code.
type ToolUseEvent struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	Context   map[string]any  `json:"context,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

// PermissionDecision is the deny payload emitted back to the hook runtime.
type PermissionDecision struct {
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// ParseToolUseEvent decodes a PreToolUse payload from Codex or Claude Code.
func ParseToolUseEvent(data []byte) (ToolUseEvent, error) {
	var event ToolUseEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return ToolUseEvent{}, fmt.Errorf("decode_tool_use_payload: %w", err)
	}
	if strings.TrimSpace(event.ToolName) == "" {
		return ToolUseEvent{}, fmt.Errorf("tool_name is required")
	}
	return event, nil
}

// ActionPayload returns the normalized tool payload sent to governance and approvals.
func (event ToolUseEvent) ActionPayload(raw []byte) []byte {
	trimmed := bytes.TrimSpace(event.ToolInput)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		return append([]byte(nil), trimmed...)
	}
	return bytes.TrimSpace(raw)
}

// ResolvedAgentID resolves the effective agent identity from hook fields and fallback config.
func (event ToolUseEvent) ResolvedAgentID(fallback string) string {
	return firstNonEmpty(
		event.AgentID,
		lookupString(event.Context, "agent_id"),
		lookupString(event.Metadata, "agent_id"),
		event.SessionID,
		lookupString(event.Context, "session_id"),
		lookupString(event.Metadata, "session_id"),
		fallback,
	)
}

// Surface resolves the logical calling surface for approval requests.
func (event ToolUseEvent) Surface(fallback string) string {
	return firstNonEmpty(
		lookupString(event.Context, "surface"),
		lookupString(event.Metadata, "surface"),
		fallback,
	)
}

// ApprovalContextJSON builds the approval context document stored with the request.
func (event ToolUseEvent) ApprovalContextJSON(raw []byte, reasons, matchedRules []string) string {
	payload := map[string]any{
		"governance_reasons": reasons,
		"hook_event":         json.RawMessage(raw),
		"matched_rules":      matchedRules,
		"tool_name":          event.ToolName,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func denyDecision(reason string) *PermissionDecision {
	return &PermissionDecision{
		PermissionDecision:       "deny",
		PermissionDecisionReason: strings.TrimSpace(reason),
	}
}

func lookupString(values map[string]any, key string) string {
	if len(values) == 0 {
		return ""
	}
	value, ok := values[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}
