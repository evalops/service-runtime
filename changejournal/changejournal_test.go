package changejournal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWriteMutationWithDollarPlaceholders(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{seq: 42}
	now := time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC)
	sequence, err := WriteMutationWithOptions(
		context.Background(),
		tx,
		Actor{Type: "service", ID: "pipeline", OrganizationID: "org-123"},
		"deal",
		"deal-1",
		"create",
		versionedPayload{Version: 7, Name: "Test"},
		map[string]any{"request_id": "req-1"},
		WriteOptions{
			Now:   func() time.Time { return now },
			NewID: func() string { return "audit-1" },
		},
	)
	if err != nil {
		t.Fatalf("write mutation: %v", err)
	}
	if sequence != 42 {
		t.Fatalf("expected sequence 42, got %d", sequence)
	}

	if !strings.Contains(tx.execQuery, "$1") {
		t.Fatalf("expected dollar placeholders in exec query, got %q", tx.execQuery)
	}
	if !strings.Contains(tx.queryRowQuery, "$9") {
		t.Fatalf("expected dollar placeholders in query row query, got %q", tx.queryRowQuery)
	}

	if tx.execArgs[0] != "audit-1" {
		t.Fatalf("unexpected audit id %#v", tx.execArgs[0])
	}
	if tx.execArgs[4] != "deal.create" {
		t.Fatalf("unexpected audit action %#v", tx.execArgs[4])
	}
	if tx.queryRowArgs[6] != int64(7) {
		t.Fatalf("unexpected aggregate version %#v", tx.queryRowArgs[6])
	}
	if tx.queryRowArgs[8] != `{"version":7,"name":"Test"}` {
		t.Fatalf("unexpected payload %#v", tx.queryRowArgs[8])
	}
}

func TestWriteMutationWithQuestionPlaceholdersAndCustomAction(t *testing.T) {
	t.Parallel()

	tx := &fakeTx{seq: 9}
	_, err := WriteMutationWithOptions(
		context.Background(),
		tx,
		Actor{Type: "api_key", OrganizationID: "org-123"},
		"contact",
		"",
		"update",
		struct {
			Name string `json:"name"`
		}{Name: "Ada"},
		nil,
		WriteOptions{
			PlaceholderStyle: PlaceholderQuestion,
			AuditAction:      "contact.manual_update",
			Now:              func() time.Time { return time.Unix(0, 0).UTC() },
			NewID:            func() string { return "audit-2" },
		},
	)
	if err != nil {
		t.Fatalf("write mutation: %v", err)
	}
	if !strings.Contains(tx.execQuery, "values (?, ?, ?, ?, ?, ?, ?, ?, ?)") {
		t.Fatalf("unexpected exec query %q", tx.execQuery)
	}
	if tx.execArgs[3] != nil {
		t.Fatalf("expected nil actor id, got %#v", tx.execArgs[3])
	}
	if tx.execArgs[4] != "contact.manual_update" {
		t.Fatalf("unexpected audit action %#v", tx.execArgs[4])
	}
	if tx.queryRowArgs[2] != nil {
		t.Fatalf("expected nil aggregate id, got %#v", tx.queryRowArgs[2])
	}
}

func TestWriteMutationMarshalErrors(t *testing.T) {
	t.Parallel()

	_, err := WriteMutation(
		context.Background(),
		&fakeTx{},
		Actor{Type: "service", OrganizationID: "org-123"},
		"deal",
		"deal-1",
		"create",
		map[string]any{"bad": make(chan int)},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "marshal_change_payload") {
		t.Fatalf("expected payload marshal error, got %v", err)
	}

	_, err = WriteMutation(
		context.Background(),
		&fakeTx{},
		Actor{Type: "service", OrganizationID: "org-123"},
		"deal",
		"deal-1",
		"create",
		map[string]string{"ok": "yes"},
		map[string]any{"bad": make(chan int)},
	)
	if err == nil || !strings.Contains(err.Error(), "marshal_audit_metadata") {
		t.Fatalf("expected metadata marshal error, got %v", err)
	}
}

func TestWriteMutationDatabaseErrors(t *testing.T) {
	t.Parallel()

	_, err := WriteMutation(
		context.Background(),
		&fakeTx{execErr: errors.New("insert failed")},
		Actor{Type: "service", OrganizationID: "org-123"},
		"deal",
		"deal-1",
		"create",
		map[string]string{"ok": "yes"},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "insert_audit_entry") {
		t.Fatalf("expected audit insert error, got %v", err)
	}

	_, err = WriteMutation(
		context.Background(),
		&fakeTx{queryRowErr: errors.New("seq failed")},
		Actor{Type: "service", OrganizationID: "org-123"},
		"deal",
		"deal-1",
		"create",
		map[string]string{"ok": "yes"},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "insert_change_journal") {
		t.Fatalf("expected change journal error, got %v", err)
	}
}

func TestTemplates(t *testing.T) {
	t.Parallel()

	dollar := Templates(PlaceholderDollar)
	if !strings.Contains(dollar.InsertAuditEntry, "$1") || !strings.Contains(dollar.InsertChangeJournal, "$9") {
		t.Fatalf("unexpected dollar templates %#v", dollar)
	}

	question := Templates(PlaceholderQuestion)
	if !strings.Contains(question.InsertAuditEntry, "?") || strings.Contains(question.InsertAuditEntry, "$1") {
		t.Fatalf("unexpected question template %#v", question.InsertAuditEntry)
	}
}

func TestChangeAndAuditEntryJSONShapes(t *testing.T) {
	t.Parallel()

	change := Change{
		Sequence:       1,
		OrganizationID: "org-123",
		AggregateType:  "deal",
		Operation:      "create",
		ActorType:      "service",
		RecordedAt:     time.Unix(0, 0).UTC(),
		Payload:        json.RawMessage(`{"ok":true}`),
	}
	audit := AuditEntry{
		ID:             "audit-1",
		OrganizationID: "org-123",
		ActorType:      "service",
		Action:         "deal.create",
		ResourceType:   "deal",
		Metadata:       json.RawMessage(`{"request_id":"req-1"}`),
		CreatedAt:      time.Unix(0, 0).UTC(),
	}

	if _, err := json.Marshal(change); err != nil {
		t.Fatalf("marshal change: %v", err)
	}
	if _, err := json.Marshal(audit); err != nil {
		t.Fatalf("marshal audit: %v", err)
	}
}

type versionedPayload struct {
	Version int64  `json:"version"`
	Name    string `json:"name"`
}

func (payload versionedPayload) GetVersion() int64 {
	return payload.Version
}

type fakeTx struct {
	execQuery     string
	execArgs      []any
	execErr       error
	queryRowQuery string
	queryRowArgs  []any
	queryRowErr   error
	seq           int64
}

func (tx *fakeTx) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	tx.execQuery = query
	tx.execArgs = append([]any(nil), args...)
	if tx.execErr != nil {
		return nil, tx.execErr
	}
	return fakeResult{}, nil
}

func (tx *fakeTx) QueryRowContext(_ context.Context, query string, args ...any) RowScanner {
	tx.queryRowQuery = query
	tx.queryRowArgs = append([]any(nil), args...)
	return fakeRow{seq: tx.seq, err: tx.queryRowErr}
}

type fakeRow struct {
	seq int64
	err error
}

func (row fakeRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	target := dest[0].(*int64)
	*target = row.seq
	return nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }
