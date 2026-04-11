package changejournal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type PlaceholderStyle int

const (
	PlaceholderDollar PlaceholderStyle = iota + 1
	PlaceholderQuestion
)

type Versioned interface {
	GetVersion() int64
}

type Actor struct {
	Type           string `json:"type"`
	ID             string `json:"id,omitempty"`
	OrganizationID string `json:"organization_id"`
}

type Change struct {
	Sequence         int64           `json:"seq"`
	OrganizationID   string          `json:"organization_id"`
	AggregateType    string          `json:"aggregate_type"`
	AggregateID      string          `json:"aggregate_id,omitempty"`
	Operation        string          `json:"operation"`
	ActorType        string          `json:"actor_type"`
	ActorID          string          `json:"actor_id,omitempty"`
	AggregateVersion int64           `json:"aggregate_version"`
	RecordedAt       time.Time       `json:"recorded_at"`
	Payload          json.RawMessage `json:"payload"`
}

type AuditEntry struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organization_id"`
	ActorType      string          `json:"actor_type"`
	ActorID        string          `json:"actor_id,omitempty"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id,omitempty"`
	Metadata       json.RawMessage `json:"metadata"`
	CreatedAt      time.Time       `json:"created_at"`
}

type SQLTemplates struct {
	InsertAuditEntry    string
	InsertChangeJournal string
}

type WriteOptions struct {
	PlaceholderStyle PlaceholderStyle
	AuditAction      string
	Now              func() time.Time
	NewID            func() string
}

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func WriteMutation(ctx context.Context, tx DBTX, actor Actor, resourceType, resourceID, operation string, payload any, metadata map[string]any) (int64, error) {
	return WriteMutationWithOptions(ctx, tx, actor, resourceType, resourceID, operation, payload, metadata, WriteOptions{})
}

func WriteMutationWithOptions(ctx context.Context, tx DBTX, actor Actor, resourceType, resourceID, operation string, payload any, metadata map[string]any, opts WriteOptions) (int64, error) {
	opts = opts.withDefaults(resourceType, operation)
	now := opts.Now().UTC()
	templates := Templates(opts.PlaceholderStyle)

	auditMetadata, err := json.Marshal(metadata)
	if err != nil {
		return 0, fmt.Errorf("marshal_audit_metadata: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal_change_payload: %w", err)
	}

	auditEntry := AuditEntry{
		ID:             opts.NewID(),
		OrganizationID: actor.OrganizationID,
		ActorType:      actor.Type,
		ActorID:        actor.ID,
		Action:         opts.AuditAction,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		Metadata:       auditMetadata,
		CreatedAt:      now,
	}

	if _, err := tx.ExecContext(
		ctx,
		templates.InsertAuditEntry,
		auditEntry.ID,
		auditEntry.OrganizationID,
		auditEntry.ActorType,
		nullableString(auditEntry.ActorID),
		auditEntry.Action,
		auditEntry.ResourceType,
		nullableString(auditEntry.ResourceID),
		string(auditEntry.Metadata),
		auditEntry.CreatedAt,
	); err != nil {
		return 0, fmt.Errorf("insert_audit_entry: %w", err)
	}

	change := Change{
		OrganizationID:   actor.OrganizationID,
		AggregateType:    resourceType,
		AggregateID:      resourceID,
		Operation:        operation,
		ActorType:        actor.Type,
		ActorID:          actor.ID,
		AggregateVersion: aggregateVersion(payload),
		RecordedAt:       now,
		Payload:          payloadJSON,
	}

	if err := tx.QueryRowContext(
		ctx,
		templates.InsertChangeJournal,
		change.OrganizationID,
		change.AggregateType,
		nullableString(change.AggregateID),
		change.Operation,
		change.ActorType,
		nullableString(change.ActorID),
		change.AggregateVersion,
		change.RecordedAt,
		string(change.Payload),
	).Scan(&change.Sequence); err != nil {
		return 0, fmt.Errorf("insert_change_journal: %w", err)
	}

	return change.Sequence, nil
}

func Templates(style PlaceholderStyle) SQLTemplates {
	style = normalizedPlaceholderStyle(style)

	return SQLTemplates{
		InsertAuditEntry: fmt.Sprintf(`
		insert into audit_entries (id, organization_id, actor_type, actor_id, action, resource_type, resource_id, metadata, created_at)
		values (%s)
	`, joinPlaceholders(style, 9)),
		InsertChangeJournal: fmt.Sprintf(`
		insert into change_journal (
			organization_id, aggregate_type, aggregate_id, operation, actor_type, actor_id, aggregate_version, recorded_at, payload
		) values (%s)
		returning seq
	`, joinPlaceholders(style, 9)),
	}
}

func (opts WriteOptions) withDefaults(resourceType, operation string) WriteOptions {
	if opts.PlaceholderStyle == 0 {
		opts.PlaceholderStyle = PlaceholderDollar
	}
	if opts.AuditAction == "" {
		opts.AuditAction = defaultAuditAction(resourceType, operation)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewID == nil {
		opts.NewID = func() string { return uuid.NewString() }
	}
	return opts
}

func aggregateVersion(payload any) int64 {
	versioned, ok := payload.(Versioned)
	if !ok {
		return 1
	}
	return versioned.GetVersion()
}

func defaultAuditAction(resourceType, operation string) string {
	resourceType = strings.TrimSpace(resourceType)
	operation = strings.TrimSpace(operation)
	switch {
	case resourceType == "":
		return operation
	case operation == "":
		return resourceType
	default:
		return resourceType + "." + operation
	}
}

func joinPlaceholders(style PlaceholderStyle, count int) string {
	parts := make([]string, 0, count)
	for index := range count {
		switch style {
		case PlaceholderQuestion:
			parts = append(parts, "?")
		default:
			parts = append(parts, fmt.Sprintf("$%d", index+1))
		}
	}
	return strings.Join(parts, ", ")
}

func normalizedPlaceholderStyle(style PlaceholderStyle) PlaceholderStyle {
	if style == PlaceholderQuestion {
		return PlaceholderQuestion
	}
	return PlaceholderDollar
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
