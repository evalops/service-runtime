package authmw

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
)

const principalContextKey contextKey = "principal"

// Principal is the normalized authenticated caller context that service
// handlers should authorize against after transport authentication succeeds.
type Principal struct {
	OrganizationID string   `json:"organization_id"`
	WorkspaceID    string   `json:"workspace_id,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	UserSubject    string   `json:"user_subject,omitempty"`
	Service        string   `json:"service,omitempty"`
	TokenType      string   `json:"token_type,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	AgentID        string   `json:"agent_id,omitempty"`
	IsHuman        bool     `json:"is_human,omitempty"`
}

// PrincipalFromActor converts the lower-level authentication actor into the
// authorization principal shape used by service handlers.
func PrincipalFromActor(actor Actor, scopes []string) Principal {
	principal := Principal{
		OrganizationID: strings.TrimSpace(actor.OrganizationID),
		Subject:        strings.TrimSpace(actor.ID),
		TokenType:      strings.TrimSpace(actor.Type),
		Scopes:         append([]string(nil), scopes...),
		IsHuman:        isHumanActorType(actor.Type),
	}
	if strings.EqualFold(principal.TokenType, "service") {
		principal.Service = principal.Subject
	}
	for key, value := range actor.Attributes {
		trimmed := strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "workspace_id", "workspace":
			principal.WorkspaceID = trimmed
		case "agent_id", "agent":
			principal.AgentID = trimmed
		case "user_subject":
			principal.UserSubject = trimmed
		case "service":
			principal.Service = trimmed
		case "token_type":
			principal.TokenType = trimmed
		case "is_human":
			principal.IsHuman = principal.IsHuman || strings.EqualFold(trimmed, "true")
		}
	}
	return principal
}

// ContextWithPrincipal stores an authenticated principal in context.
func ContextWithPrincipal(ctx context.Context, principal Principal) context.Context {
	principal.Scopes = append([]string(nil), principal.Scopes...)
	return context.WithValue(ctx, principalContextKey, principal)
}

// PrincipalFromContext retrieves the authenticated principal from context.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey).(Principal)
	if !ok {
		return Principal{}, false
	}
	principal.Scopes = append([]string(nil), principal.Scopes...)
	return principal, true
}

// RequireOrganization returns PermissionDenied when the caller is not scoped to
// the requested organization.
func (p Principal) RequireOrganization(id string) error {
	target := strings.TrimSpace(id)
	if target == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("organization_id is required"))
	}
	if strings.TrimSpace(p.OrganizationID) != target {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("organization %s not authorized", target))
	}
	return nil
}

// RequireWorkspace returns PermissionDenied when the caller is not scoped to
// the requested workspace.
func (p Principal) RequireWorkspace(id string) error {
	target := strings.TrimSpace(id)
	if target == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("workspace_id is required"))
	}
	if strings.TrimSpace(p.WorkspaceID) != target {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("workspace %s not authorized", target))
	}
	return nil
}

// RequireScope returns PermissionDenied when the caller lacks scope.
func (p Principal) RequireScope(scope string) error {
	scope = strings.TrimSpace(scope)
	if scope == "" || slices.Contains(p.Scopes, scope) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("missing scope %q", scope))
}

// RequireHuman rejects service and API-key principals for human-only actions.
func (p Principal) RequireHuman() error {
	if p.IsHuman || isHumanActorType(p.TokenType) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("human principal required"))
}

// RejectSelfApproval rejects attempts by an agent to approve its own request.
func (p Principal) RejectSelfApproval(agentID string) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil
	}
	for _, candidate := range []string{p.AgentID, p.Subject, p.UserSubject} {
		if strings.TrimSpace(candidate) == agentID {
			return connect.NewError(connect.CodePermissionDenied, errors.New("principal cannot approve its own request"))
		}
	}
	return nil
}

func isHumanActorType(actorType string) bool {
	switch strings.ToLower(strings.TrimSpace(actorType)) {
	case "human", "user":
		return true
	default:
		return false
	}
}
