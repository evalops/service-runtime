package changejournal

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	eventsv1 "github.com/evalops/proto/gen/go/events/v1"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestWriteMutationWithDollarPlaceholders(t *testing.T) {
	t.Parallel()

	db, mock := newMockDB(t)
	now := time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC)
	templates := Templates(PlaceholderDollar)
	if !strings.Contains(templates.InsertAuditEntry, "$1") {
		t.Fatalf("expected dollar placeholders in exec query, got %q", templates.InsertAuditEntry)
	}
	if !strings.Contains(templates.InsertChangeJournal, "$9") {
		t.Fatalf("expected dollar placeholders in query row query, got %q", templates.InsertChangeJournal)
	}

	mock.ExpectExec(quoteQuery(templates.InsertAuditEntry)).
		WithArgs(
			"audit-1",
			"org-123",
			"service",
			"pipeline",
			"deal.create",
			"deal",
			"deal-1",
			`{"request_id":"req-1"}`,
			now,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(quoteQuery(templates.InsertChangeJournal)).
		WithArgs(
			"org-123",
			"deal",
			"deal-1",
			"create",
			"service",
			"pipeline",
			int64(7),
			now,
			`{"version":7,"name":"Test"}`,
		).
		WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(42))

	sequence, err := WriteMutationWithOptions(
		context.Background(),
		db,
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
}

func TestWriteMutationWithQuestionPlaceholdersAndCustomAction(t *testing.T) {
	t.Parallel()

	db, mock := newMockDB(t)
	templates := Templates(PlaceholderQuestion)
	if !strings.Contains(templates.InsertAuditEntry, "values (?, ?, ?, ?, ?, ?, ?, ?, ?)") {
		t.Fatalf("unexpected exec query %q", templates.InsertAuditEntry)
	}

	now := time.Unix(0, 0).UTC()
	mock.ExpectExec(quoteQuery(templates.InsertAuditEntry)).
		WithArgs(
			"audit-2",
			"org-123",
			"api_key",
			nil,
			"contact.manual_update",
			"contact",
			nil,
			"null",
			now,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(quoteQuery(templates.InsertChangeJournal)).
		WithArgs(
			"org-123",
			"contact",
			nil,
			"update",
			"api_key",
			nil,
			int64(1),
			now,
			`{"name":"Ada"}`,
		).
		WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(9))

	_, err := WriteMutationWithOptions(
		context.Background(),
		db,
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
			Now:              func() time.Time { return now },
			NewID:            func() string { return "audit-2" },
		},
	)
	if err != nil {
		t.Fatalf("write mutation: %v", err)
	}
}

func TestWriteMutationMarshalErrors(t *testing.T) {
	t.Parallel()

	db, _ := newMockDB(t)
	_, err := WriteMutation(
		context.Background(),
		db,
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
		db,
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

func TestWriteMutationMarshalsProtoPayloadWithTypeURL(t *testing.T) {
	t.Parallel()

	db, mock := newMockDB(t)
	templates := Templates(PlaceholderDollar)
	now := time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC)

	mock.ExpectExec(quoteQuery(templates.InsertAuditEntry)).
		WithArgs(
			"audit-3",
			"org-123",
			"service",
			nil,
			"deal.update",
			"deal",
			"deal-1",
			"null",
			now,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(quoteQuery(templates.InsertChangeJournal)).
		WithArgs(
			"org-123",
			"deal",
			"deal-1",
			"update",
			"service",
			nil,
			int64(1),
			now,
			jsonStringArg(t, `{"@type":"type.googleapis.com/google.protobuf.StringValue", "value":"d-1"}`),
		).
		WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(12))

	sequence, err := WriteMutationWithOptions(
		context.Background(),
		db,
		Actor{Type: "service", OrganizationID: "org-123"},
		"deal",
		"deal-1",
		"update",
		wrapperspb.String("d-1"),
		nil,
		WriteOptions{
			Now:   func() time.Time { return now },
			NewID: func() string { return "audit-3" },
		},
	)
	if err != nil {
		t.Fatalf("write mutation: %v", err)
	}
	if sequence != 12 {
		t.Fatalf("expected sequence 12, got %d", sequence)
	}
}

type jsonArgument struct {
	expected string
}

func (arg jsonArgument) Match(value driver.Value) bool {
	actual, ok := value.(string)
	if !ok {
		return false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(actual)); err != nil {
		return false
	}
	return buf.String() == arg.expected
}

func jsonStringArg(t *testing.T, value string) sqlmock.Argument {
	t.Helper()
	return jsonArgument{expected: compactJSON(t, value)}
}

func compactJSON(t *testing.T, value string) string {
	t.Helper()

	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(value)); err != nil {
		t.Fatalf("compact json: %v", err)
	}
	return buf.String()
}

func TestWriteMutationDatabaseErrors(t *testing.T) {
	t.Parallel()

	db, mock := newMockDB(t)
	templates := Templates(PlaceholderDollar)

	mock.ExpectExec(quoteQuery(templates.InsertAuditEntry)).
		WithArgs(
			sqlmock.AnyArg(),
			"org-123",
			"service",
			nil,
			"deal.create",
			"deal",
			"deal-1",
			"null",
			sqlmock.AnyArg(),
		).
		WillReturnError(errors.New("insert failed"))

	_, err := WriteMutation(
		context.Background(),
		db,
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

	db, mock = newMockDB(t)
	templates = Templates(PlaceholderDollar)
	mock.ExpectExec(quoteQuery(templates.InsertAuditEntry)).
		WithArgs(
			sqlmock.AnyArg(),
			"org-123",
			"service",
			nil,
			"deal.create",
			"deal",
			"deal-1",
			"null",
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(quoteQuery(templates.InsertChangeJournal)).
		WithArgs(
			"org-123",
			"deal",
			"deal-1",
			"create",
			"service",
			nil,
			int64(1),
			sqlmock.AnyArg(),
			`{"ok":"yes"}`,
		).
		WillReturnError(errors.New("seq failed"))

	_, err = WriteMutation(
		context.Background(),
		db,
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

func TestChangeProtoRoundTrip(t *testing.T) {
	t.Parallel()

	change := Change{
		Sequence:         7,
		OrganizationID:   "org-123",
		AggregateType:    "deal",
		AggregateID:      "deal-1",
		Operation:        "update",
		ActorType:        "service",
		ActorID:          "pipeline",
		AggregateVersion: 9,
		RecordedAt:       time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
		Payload:          json.RawMessage(`{"name":"Ada","version":9}`),
	}

	message, err := change.ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	if message.GetOrganizationId() != "org-123" {
		t.Fatalf("unexpected organization id %q", message.GetOrganizationId())
	}
	if message.GetPayload().AsMap()["name"] != "Ada" {
		t.Fatalf("unexpected payload %#v", message.GetPayload().AsMap())
	}

	decoded, err := ChangeFromProto(message)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	if decoded.Sequence != change.Sequence {
		t.Fatalf("unexpected sequence %d", decoded.Sequence)
	}
	if decoded.AggregateID != change.AggregateID {
		t.Fatalf("unexpected aggregate id %q", decoded.AggregateID)
	}
	if compactJSON(t, string(decoded.Payload)) != compactJSON(t, string(change.Payload)) {
		t.Fatalf("unexpected payload %s", decoded.Payload)
	}
}

func TestChangeToProtoPreservesNilPayload(t *testing.T) {
	t.Parallel()

	message, err := (Change{}).ToProto()
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}
	if message.GetPayload() != nil {
		t.Fatalf("expected nil payload, got %#v", message.GetPayload())
	}

	decoded, err := ChangeFromProto(&eventsv1.Change{})
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	if decoded.Payload != nil {
		t.Fatalf("expected nil payload, got %#v", decoded.Payload)
	}
}

func TestChangeToProtoRejectsNonObjectPayload(t *testing.T) {
	t.Parallel()

	_, err := (Change{Payload: json.RawMessage(`["not","an","object"]`)}).ToProto()
	if err == nil || !strings.Contains(err.Error(), "change_payload_not_object") {
		t.Fatalf("expected non-object payload error, got %v", err)
	}
}

type versionedPayload struct {
	Version int64  `json:"version"`
	Name    string `json:"name"`
}

func (payload versionedPayload) GetVersion() int64 {
	return payload.Version
}

func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})
	return db, mock
}

func quoteQuery(query string) string {
	return regexp.QuoteMeta(query)
}
