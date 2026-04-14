package featureflags

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	configv1 "github.com/evalops/proto/gen/go/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// DefaultPollInterval is the default delay between filesystem refresh checks.
const DefaultPollInterval = 5 * time.Second

// Options configures a FileStore.
type Options struct {
	Logger       *slog.Logger
	PollInterval time.Duration
}

// FileStore reads a shared protojson feature-flag snapshot from disk.
type FileStore struct {
	logger       *slog.Logger
	path         string
	pollInterval time.Duration
	now          func() time.Time
	readFile     func(string) ([]byte, error)
	stat         func(string) (os.FileInfo, error)

	mu          sync.RWMutex
	flags       map[string]*configv1.FeatureFlag
	lastChecked time.Time
	lastModTime time.Time
	snapshot    *configv1.FeatureFlagSnapshot
}

// NewFileStore loads a feature-flag snapshot from path and refreshes it lazily.
func NewFileStore(path string, opts Options) (*FileStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("feature_flags_path_required")
	}

	store := &FileStore{
		logger:       opts.Logger,
		path:         path,
		pollInterval: opts.PollInterval,
		now:          time.Now,
		readFile:     os.ReadFile,
		stat:         os.Stat,
	}
	if store.pollInterval <= 0 {
		store.pollInterval = DefaultPollInterval
	}

	if err := store.reload(); err != nil {
		return nil, err
	}

	return store, nil
}

// Enabled reports whether the named flag is present and enabled.
func (store *FileStore) Enabled(key string) bool {
	flag, ok := store.Lookup(key)
	return ok && flag.GetEnabled()
}

// EnabledFor reports whether the named flag is enabled for a specific subject.
// When rollout_percent is unset, enabled flags default to full rollout.
// Gradual rollouts require a non-empty subject.
func (store *FileStore) EnabledFor(key string, subject string) bool {
	flag, ok := store.Lookup(key)
	if !ok || !flag.GetEnabled() {
		return false
	}

	rolloutPercent := effectiveRolloutPercent(flag.GetRolloutPercent())
	if rolloutPercent >= 100 {
		return true
	}

	subject = strings.TrimSpace(subject)
	if subject == "" {
		return false
	}

	return rolloutBucket(key, subject) < rolloutPercent
}

// HasExplicitRollout reports whether the named flag has a non-zero rollout_percent,
// regardless of whether the flag is currently enabled. This distinguishes flags that
// use gradual rollout semantics from simple on/off flags.
func (store *FileStore) HasExplicitRollout(key string) bool {
	flag, ok := store.Lookup(key)
	return ok && flag.GetRolloutPercent() > 0
}

// Lookup returns a cloned copy of the named flag when present.
func (store *FileStore) Lookup(key string) (*configv1.FeatureFlag, bool) {
	if store == nil {
		return nil, false
	}
	store.refreshIfNeeded()

	store.mu.RLock()
	defer store.mu.RUnlock()

	flag, ok := store.flags[strings.TrimSpace(key)]
	if !ok {
		return nil, false
	}
	cloned, ok := proto.Clone(flag).(*configv1.FeatureFlag)
	if !ok || cloned == nil {
		return nil, false
	}
	return cloned, true
}

// Snapshot returns a cloned copy of the current in-memory snapshot.
func (store *FileStore) Snapshot() *configv1.FeatureFlagSnapshot {
	if store == nil {
		return &configv1.FeatureFlagSnapshot{}
	}
	store.refreshIfNeeded()

	store.mu.RLock()
	defer store.mu.RUnlock()

	if store.snapshot == nil {
		return &configv1.FeatureFlagSnapshot{}
	}
	cloned, ok := proto.Clone(store.snapshot).(*configv1.FeatureFlagSnapshot)
	if !ok || cloned == nil {
		return &configv1.FeatureFlagSnapshot{}
	}
	return cloned
}

func (store *FileStore) refreshIfNeeded() {
	now := store.now()

	store.mu.RLock()
	shouldReload := store.lastChecked.IsZero() || now.Sub(store.lastChecked) >= store.pollInterval
	store.mu.RUnlock()
	if !shouldReload {
		return
	}

	store.mu.Lock()
	if !store.lastChecked.IsZero() && now.Sub(store.lastChecked) < store.pollInterval {
		store.mu.Unlock()
		return
	}
	store.lastChecked = now
	store.mu.Unlock()

	if err := store.reload(); err != nil {
		store.logWarn("reload feature flags failed", "path", store.path, "error", err)
	}
}

func (store *FileStore) reload() error {
	info, err := store.stat(store.path)
	if err != nil {
		return fmt.Errorf("stat_feature_flags: %w", err)
	}

	store.mu.RLock()
	lastModTime := store.lastModTime
	store.mu.RUnlock()
	if !lastModTime.IsZero() && !info.ModTime().After(lastModTime) {
		return nil
	}

	contents, err := store.readFile(store.path)
	if err != nil {
		return fmt.Errorf("read_feature_flags: %w", err)
	}

	var snapshot configv1.FeatureFlagSnapshot
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(contents, &snapshot); err != nil {
		return fmt.Errorf("unmarshal_feature_flags: %w", err)
	}

	flags := make(map[string]*configv1.FeatureFlag, len(snapshot.GetFlags()))
	for _, flag := range snapshot.GetFlags() {
		if flag == nil {
			continue
		}
		key := strings.TrimSpace(flag.GetKey())
		if key == "" {
			continue
		}
		cloned, ok := proto.Clone(flag).(*configv1.FeatureFlag)
		if !ok || cloned == nil {
			continue
		}
		flags[key] = cloned
	}

	clonedSnapshot, ok := proto.Clone(&snapshot).(*configv1.FeatureFlagSnapshot)
	if !ok || clonedSnapshot == nil {
		return fmt.Errorf("clone_feature_flags_snapshot")
	}

	store.mu.Lock()
	store.flags = flags
	store.lastModTime = info.ModTime()
	store.snapshot = clonedSnapshot
	store.mu.Unlock()

	return nil
}

func (store *FileStore) logWarn(msg string, args ...any) {
	if store != nil && store.logger != nil {
		store.logger.Warn(msg, args...)
	}
}

// effectiveRolloutPercent returns the effective rollout percentage for a flag.
// Proto3 scalar fields lack presence tracking, so 0 and "unset" are
// indistinguishable. By convention, 0 is treated as 100% (full rollout).
// To disable a flag, set enabled=false rather than rollout_percent=0.
// Values above 100 are clamped to 100.
func effectiveRolloutPercent(value uint32) uint32 {
	switch {
	case value == 0:
		return 100
	case value > 100:
		return 100
	default:
		return value
	}
}

func rolloutBucket(key string, subject string) uint32 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(strings.TrimSpace(key)))
	_, _ = hasher.Write([]byte{':'})
	_, _ = hasher.Write([]byte(strings.TrimSpace(subject)))
	return hasher.Sum32() % 100
}
