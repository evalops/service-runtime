package agenthook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ToolUseEvent is the generic hook envelope used by Codex and Claude Code.
type ToolUseEvent struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	Context   map[string]any  `json:"context,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

// PermissionDecision is the hook response shape that blocks tool execution.
type PermissionDecision struct {
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// ParseToolUseEvent decodes the incoming PreToolUse payload.
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

func (event ToolUseEvent) actionPayload(raw []byte) []byte {
	trimmed := bytes.TrimSpace(event.ToolInput)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		return append([]byte(nil), trimmed...)
	}
	return bytes.TrimSpace(raw)
}

func (event ToolUseEvent) resolvedAgentID(fallback string) string {
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

func (event ToolUseEvent) surface(fallback string) string {
	return firstNonEmpty(
		lookupString(event.Context, "surface"),
		lookupString(event.Metadata, "surface"),
		fallback,
	)
}

func (event ToolUseEvent) approvalContextJSON(raw []byte, reasons, matchedRules []string) string {
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
