package idempotency

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/evalops/service-runtime/authmw"
)

func TestRequestHash(t *testing.T) {
	t.Parallel()

	left := RequestHash(http.MethodPost, "/deals", []byte(`{"a":1}`))
	right := RequestHash(http.MethodPost, "/deals", []byte(`{"a":1}`))
	other := RequestHash(http.MethodPost, "/deals", []byte(`{"a":2}`))

	if left != right {
		t.Fatal("expected stable hash")
	}
	if left == other {
		t.Fatal("expected different body to change hash")
	}
}

func TestDefaultScope(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/deals", nil)
	request.Header.Set("Authorization", "Bearer svc-token")

	var (
		scope string
		err   error
	)
	handler := withAuthenticatedActor(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		scope, err = DefaultScope(request)
		writer.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if err != nil {
		t.Fatalf("default scope: %v", err)
	}
	if scope != "org-123:POST:/deals" {
		t.Fatalf("unexpected scope %q", scope)
	}
}

func TestMiddlewareMissingKey(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	recorder := httptest.NewRecorder()
	handler := Middleware(store, time.Hour)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next should not run")
	}))
	handler.ServeHTTP(recorder, requestWithActor(http.MethodPost, "/deals", `{"ok":true}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestMiddlewareReplay(t *testing.T) {
	t.Parallel()

	store := &fakeStore{
		replay: &ReplayResult{
			StatusCode:  http.StatusCreated,
			ContentType: "application/json",
			Body:        []byte(`{"id":"1"}`),
		},
	}

	recorder := httptest.NewRecorder()
	handler := Middleware(store, time.Hour)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next should not run on replay")
	}))

	request := requestWithActor(http.MethodPost, "/deals", `{"ok":true}`)
	request.Header.Set("Idempotency-Key", "idem-1")
	withAuthenticatedActor(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, recorder.Code)
	}
	if recorder.Header().Get("X-Idempotent-Replay") != "true" {
		t.Fatal("expected replay header")
	}
	if !store.cleanupCalled {
		t.Fatal("expected cleanup call")
	}
}

func TestMiddlewareConflictAndPending(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "conflict", err: ErrConflict, status: http.StatusConflict},
		{name: "pending", err: ErrPending, status: http.StatusConflict},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeStore{beginErr: testCase.err}
			recorder := httptest.NewRecorder()
			handler := Middleware(store, time.Hour)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next should not run")
			}))

			request := requestWithActor(http.MethodPost, "/deals", `{"ok":true}`)
			request.Header.Set("Idempotency-Key", "idem-1")
			withAuthenticatedActor(handler).ServeHTTP(recorder, request)

			if recorder.Code != testCase.status {
				t.Fatalf("expected status %d, got %d", testCase.status, recorder.Code)
			}
		})
	}
}

func TestMiddlewareSuccessCompletesStoredResponse(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	store := &fakeStore{}
	handler := MiddlewareWithOptions(store, Options{
		TTL:    time.Hour,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Now:    func() time.Time { return time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC) },
	})(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"ok":true}` {
			t.Fatalf("expected restored request body, got %q", string(body))
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"id":"1"}`))
	}))

	recorder := httptest.NewRecorder()
	request := requestWithActor(http.MethodPost, "/deals", `{"ok":true}`)
	request.Header.Set("Idempotency-Key", "idem-1")
	withAuthenticatedActor(handler).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, recorder.Code)
	}
	if store.completeResult.StatusCode != http.StatusCreated {
		t.Fatalf("unexpected complete status %#v", store.completeResult.StatusCode)
	}
	if store.scope != "org-123:POST:/deals" {
		t.Fatalf("unexpected scope %q", store.scope)
	}
	if store.beginHash == "" {
		t.Fatal("expected request hash to be recorded")
	}
}

func TestPostgresStoreLifecycle(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewPostgresStore(db)
	now := time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC)

	mock.ExpectExec("delete from api_idempotency_keys").WithArgs(now.UTC()).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Cleanup(context.Background(), now); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery("select request_hash, response_code, response_body, content_type").
		WithArgs("scope", "key").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("insert into api_idempotency_keys").
		WithArgs("scope", "key", "hash", now.UTC(), now.UTC().Add(time.Hour)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	replay, err := store.Begin(context.Background(), "scope", "key", "hash", time.Hour, now)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if replay != nil {
		t.Fatalf("expected no replay, got %#v", replay)
	}

	mock.ExpectExec("update api_idempotency_keys").
		WithArgs(http.StatusCreated, []byte(`{"id":"1"}`), "application/json", now.UTC(), "scope", "key").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Complete(context.Background(), "scope", "key", ReplayResult{
		StatusCode:  http.StatusCreated,
		ContentType: "application/json",
		Body:        []byte(`{"id":"1"}`),
	}, now); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPostgresStoreReplayConflictAndPending(t *testing.T) {
	t.Parallel()

	t.Run("conflict", func(t *testing.T) {
		db, mock, _ := sqlmock.New()
		defer db.Close()

		store := NewPostgresStore(db)
		mock.ExpectBegin()
		mock.ExpectQuery("select request_hash, response_code, response_body, content_type").
			WithArgs("scope", "key").
			WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_code", "response_body", "content_type"}).
				AddRow("other-hash", nil, []byte(nil), nil))
		mock.ExpectRollback()

		_, err := store.Begin(context.Background(), "scope", "key", "hash", time.Hour, time.Now())
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("expected ErrConflict, got %v", err)
		}
	})

	t.Run("pending", func(t *testing.T) {
		db, mock, _ := sqlmock.New()
		defer db.Close()

		store := NewPostgresStore(db)
		mock.ExpectBegin()
		mock.ExpectQuery("select request_hash, response_code, response_body, content_type").
			WithArgs("scope", "key").
			WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_code", "response_body", "content_type"}).
				AddRow("hash", nil, []byte(nil), nil))
		mock.ExpectRollback()

		_, err := store.Begin(context.Background(), "scope", "key", "hash", time.Hour, time.Now())
		if !errors.Is(err, ErrPending) {
			t.Fatalf("expected ErrPending, got %v", err)
		}
	})
}

func requestWithActor(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer svc-token")
	return request
}

func withAuthenticatedActor(next http.Handler) http.Handler {
	return authmw.New(authmw.Config{
		TokenVerifier: stubTokenVerifier{
			result: authmw.VerifiedToken{
				Actor: authmw.Actor{
					Type:           "service",
					ID:             "pipeline",
					OrganizationID: "org-123",
				},
			},
		},
	}).WithAuth("scope:write")(next)
}

type fakeStore struct {
	replay         *ReplayResult
	beginErr       error
	cleanupCalled  bool
	scope          string
	beginHash      string
	completeResult ReplayResult
}

func (store *fakeStore) Cleanup(context.Context, time.Time) error {
	store.cleanupCalled = true
	return nil
}

func (store *fakeStore) Begin(_ context.Context, scope, key, requestHash string, _ time.Duration, _ time.Time) (*ReplayResult, error) {
	store.scope = scope
	store.beginHash = requestHash
	return store.replay, store.beginErr
}

func (store *fakeStore) Complete(_ context.Context, scope, key string, result ReplayResult, _ time.Time) error {
	store.scope = scope
	store.completeResult = result
	return nil
}

type stubTokenVerifier struct {
	result authmw.VerifiedToken
}

func (verifier stubTokenVerifier) VerifyToken(context.Context, string, []string) (authmw.VerifiedToken, error) {
	return verifier.result, nil
}
