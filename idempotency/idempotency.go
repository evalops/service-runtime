package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/evalops/service-runtime/authmw"
	"github.com/evalops/service-runtime/httpkit"
)

// DefaultTTL is the default time-to-live for idempotency keys.
const DefaultTTL = 24 * time.Hour

// ErrConflict, ErrPending, and ErrScopeUnavailable are sentinel errors returned by the idempotency store.
var (
	ErrConflict         = errors.New("idempotency_conflict")
	ErrPending          = errors.New("idempotency_pending")
	ErrScopeUnavailable = errors.New("idempotency_scope_unavailable")
)

// ReplayResult holds the stored HTTP response for an idempotent replay.
type ReplayResult struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// Store persists and retrieves idempotency keys.
type Store interface {
	Cleanup(ctx context.Context, now time.Time) error
	Begin(ctx context.Context, scope, key, requestHash string, ttl time.Duration, now time.Time) (*ReplayResult, error)
	Complete(ctx context.Context, scope, key string, result ReplayResult, completedAt time.Time) error
}

// ScopeFunc derives the idempotency scope (e.g., org+method+path) from a request.
type ScopeFunc func(request *http.Request) (string, error)

// DefaultCleanupInterval is the default interval between store cleanup runs.
const DefaultCleanupInterval = 5 * time.Minute

// Options configures the idempotency middleware.
type Options struct {
	TTL             time.Duration
	CleanupInterval time.Duration // minimum interval between cleanup runs (default: 5m)
	ScopeFunc       ScopeFunc
	Logger          *slog.Logger
	Now             func() time.Time
}

// Middleware returns an HTTP middleware that enforces idempotency using the given store and TTL.
func Middleware(store Store, ttl time.Duration) func(http.Handler) http.Handler {
	return MiddlewareWithOptions(store, Options{TTL: ttl})
}

// MiddlewareWithOptions is like Middleware but accepts a full Options struct.
func MiddlewareWithOptions(store Store, opts Options) func(http.Handler) http.Handler {
	opts = opts.withDefaults()

	var (
		lastCleanupMu sync.Mutex
		lastCleanup   time.Time
	)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
			if key == "" {
				httpkit.WriteError(writer, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
				return
			}

			scope, err := opts.ScopeFunc(request)
			if err != nil {
				httpkit.WriteError(writer, http.StatusInternalServerError, "idempotency_scope_failed", err.Error())
				return
			}

			body, err := io.ReadAll(request.Body)
			if err != nil {
				httpkit.WriteError(writer, http.StatusBadRequest, "invalid_body", "unable to read request body")
				return
			}
			_ = request.Body.Close()
			request.Body = io.NopCloser(bytes.NewReader(body))

			now := opts.Now()
			cleanupNow := now.UTC()

			lastCleanupMu.Lock()
			shouldCleanup := lastCleanup.IsZero() || now.Sub(lastCleanup) >= opts.CleanupInterval
			if shouldCleanup {
				lastCleanup = now
			}
			lastCleanupMu.Unlock()
			if shouldCleanup {
				if err := store.Cleanup(request.Context(), cleanupNow); err != nil {
					opts.Logger.Warn("idempotency cleanup failed", "error", err)
				}
			}

			hash := RequestHash(request.Method, request.URL.Path, body)
			replay, err := store.Begin(request.Context(), scope, key, hash, opts.TTL, cleanupNow)
			switch {
			case err == nil:
			case errors.Is(err, ErrConflict):
				httpkit.WriteError(writer, http.StatusConflict, "idempotency_conflict", "Idempotency key already exists for a different request")
				return
			case errors.Is(err, ErrPending):
				httpkit.WriteError(writer, http.StatusConflict, "idempotency_in_progress", "A matching request is still in progress")
				return
			default:
				httpkit.WriteError(writer, http.StatusInternalServerError, "idempotency_failed", err.Error())
				return
			}

			if replay != nil {
				if replay.ContentType != "" {
					writer.Header().Set("Content-Type", replay.ContentType)
				}
				writer.Header().Set("X-Idempotent-Replay", "true")
				writer.WriteHeader(replay.StatusCode)
				//nolint:gosec // G705: body is our own stored handler response, not user-reflected input.
				// #nosec G705 -- Replayed bytes come from our stored handler response, not direct user input.
				_, _ = writer.Write(replay.Body)
				return
			}

			recorder := httpkit.NewCaptureResponseWriter(writer)
			next.ServeHTTP(recorder, request)

			contentType := recorder.Header().Get("Content-Type")
			if contentType == "" {
				contentType = "application/json"
			}
			if err := store.Complete(request.Context(), scope, key, ReplayResult{
				StatusCode:  recorder.StatusCode(),
				ContentType: contentType,
				Body:        recorder.BodyBytes(),
			}, opts.Now().UTC()); err != nil {
				opts.Logger.Error("failed to persist idempotent response", "error", err, "scope", scope)
			}
		})
	}
}

// DefaultScope derives the idempotency scope from the authenticated actor ID, method, and path.
func DefaultScope(request *http.Request) (string, error) {
	actor, ok := authmw.ActorFromContext(request.Context())
	if !ok || strings.TrimSpace(actor.OrganizationID) == "" {
		return "", ErrScopeUnavailable
	}
	return actor.OrganizationID + ":" + request.Method + ":" + request.URL.Path, nil
}

// RequestHash returns a hex-encoded SHA-256 of the method, path, and body.
func RequestHash(method, path string, body []byte) string {
	hash := sha256.New()
	hash.Write([]byte(method + ":" + path + ":"))
	hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}

func (opts Options) withDefaults() Options {
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.CleanupInterval <= 0 {
		opts.CleanupInterval = DefaultCleanupInterval
	}
	if opts.ScopeFunc == nil {
		opts.ScopeFunc = DefaultScope
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return opts
}

// PostgresStore is a Store backed by a Postgres database.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore creates a PostgresStore using the given database connection.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Cleanup deletes expired idempotency keys.
func (store *PostgresStore) Cleanup(ctx context.Context, now time.Time) error {
	_, err := store.db.ExecContext(ctx, deleteExpiredIdempotencyKeysSQL, now.UTC())
	return err
}

// Begin registers a new idempotency key or returns the stored replay result if it already exists.
func (store *PostgresStore) Begin(ctx context.Context, scope, key, requestHash string, ttl time.Duration, now time.Time) (*ReplayResult, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)

	var storedHash string
	var responseCode sql.NullInt64
	var body []byte
	var contentType sql.NullString
	err = tx.QueryRowContext(ctx, getIdempotencyKeySQL, scope, key).Scan(&storedHash, &responseCode, &body, &contentType)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, insertIdempotencyKeySQL, scope, key, requestHash, now.UTC(), now.UTC().Add(ttl)); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	case err != nil:
		return nil, err
	}

	if storedHash != requestHash {
		return nil, ErrConflict
	}
	if !responseCode.Valid {
		return nil, ErrPending
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &ReplayResult{
		StatusCode:  int(responseCode.Int64),
		ContentType: contentType.String,
		Body:        append([]byte(nil), body...),
	}, nil
}

// Complete stores the response for a completed idempotency key.
func (store *PostgresStore) Complete(ctx context.Context, scope, key string, result ReplayResult, completedAt time.Time) error {
	_, err := store.db.ExecContext(
		ctx,
		completeIdempotencyKeySQL,
		result.StatusCode,
		result.Body,
		result.ContentType,
		completedAt.UTC(),
		scope,
		key,
	)
	return err
}

func rollback(tx *sql.Tx) {
	if tx != nil {
		_ = tx.Rollback()
	}
}

const (
	deleteExpiredIdempotencyKeysSQL = `delete from api_idempotency_keys where expires_at <= $1`
	getIdempotencyKeySQL            = `
		select request_hash, response_code, response_body, content_type
		from api_idempotency_keys
		where scope = $1 and idempotency_key = $2
	`
	insertIdempotencyKeySQL = `
		insert into api_idempotency_keys (
			scope, idempotency_key, request_hash, created_at, expires_at
		) values ($1, $2, $3, $4, $5)
	`
	completeIdempotencyKeySQL = `
		update api_idempotency_keys
		set response_code = $1, response_body = $2, content_type = $3, completed_at = $4
		where scope = $5 and idempotency_key = $6
	`
)
