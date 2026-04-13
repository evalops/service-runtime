package featureflags

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	configv1 "github.com/evalops/proto/gen/go/config/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestFileStoreEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.json")
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: "platform.kill_switches.llm_gateway.inference", Enabled: true},
			{Key: "llm_gateway.model_routing.provider_failover", Enabled: false},
		},
	})

	store, err := NewFileStore(path, Options{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	if !store.Enabled("platform.kill_switches.llm_gateway.inference") {
		t.Fatalf("expected kill switch flag to be enabled")
	}
	if store.Enabled("llm_gateway.model_routing.provider_failover") {
		t.Fatalf("expected provider failover flag to be disabled")
	}
	if store.Enabled("missing.flag") {
		t.Fatalf("expected missing flag to be disabled")
	}
}

func TestFileStoreEnabledForRolloutPercent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.json")
	const flagKey = "llm_gateway.model_routing.provider_failover"
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: flagKey, Enabled: true, RolloutPercent: 30},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	var (
		inRolloutSubject  string
		outRolloutSubject string
	)
	for _, subject := range []string{"org-alpha", "org-bravo", "org-charlie", "org-delta", "org-echo"} {
		if rolloutBucket(flagKey, subject) < 30 && inRolloutSubject == "" {
			inRolloutSubject = subject
		}
		if rolloutBucket(flagKey, subject) >= 30 && outRolloutSubject == "" {
			outRolloutSubject = subject
		}
	}
	if inRolloutSubject == "" || outRolloutSubject == "" {
		t.Fatalf("expected deterministic test subjects for rollout buckets")
	}

	if !store.EnabledFor(flagKey, inRolloutSubject) {
		t.Fatalf("expected %q to be inside rollout", inRolloutSubject)
	}
	if store.EnabledFor(flagKey, outRolloutSubject) {
		t.Fatalf("expected %q to be outside rollout", outRolloutSubject)
	}
}

func TestFileStoreEnabledForBlankSubjectUsesFullFlagState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.json")
	const flagKey = "platform.kill_switches.prompts.resolve_api"
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: flagKey, Enabled: true, RolloutPercent: 10},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	if !store.EnabledFor(flagKey, "") {
		t.Fatalf("expected blank subject to honor enabled state")
	}
}

func TestFileStoreReloadsUpdatedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.json")
	now := time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC)
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: "platform.kill_switches.llm_gateway.inference", Enabled: false},
		},
	})
	touchFile(t, path, now)

	store, err := NewFileStore(path, Options{PollInterval: time.Second})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	store.now = func() time.Time { return now }

	if store.Enabled("platform.kill_switches.llm_gateway.inference") {
		t.Fatalf("expected initial kill switch state to be disabled")
	}

	now = now.Add(2 * time.Second)
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: "platform.kill_switches.llm_gateway.inference", Enabled: true},
		},
	})
	touchFile(t, path, now)

	if !store.Enabled("platform.kill_switches.llm_gateway.inference") {
		t.Fatalf("expected updated kill switch state to be enabled")
	}
}

func TestFileStoreKeepsLastGoodSnapshotOnReloadFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.json")
	now := time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC)
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: "platform.kill_switches.llm_gateway.inference", Enabled: true},
		},
	})
	touchFile(t, path, now)

	store, err := NewFileStore(path, Options{PollInterval: time.Second})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	store.now = func() time.Time { return now }

	if !store.Enabled("platform.kill_switches.llm_gateway.inference") {
		t.Fatalf("expected initial kill switch state to be enabled")
	}

	now = now.Add(2 * time.Second)
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write invalid snapshot: %v", err)
	}
	touchFile(t, path, now)

	if !store.Enabled("platform.kill_switches.llm_gateway.inference") {
		t.Fatalf("expected reload failure to preserve last good snapshot")
	}
}

func TestNewFileStoreRequiresPath(t *testing.T) {
	if _, err := NewFileStore("", Options{}); err == nil {
		t.Fatalf("expected missing path to fail")
	}
}

func touchFile(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("touch %s: %v", path, err)
	}
}

func writeSnapshot(t *testing.T, path string, snapshot *configv1.FeatureFlagSnapshot) {
	t.Helper()
	payload, err := protojson.MarshalOptions{
		Indent:          "  ",
		UseProtoNames:   true,
		EmitUnpopulated: true,
	}.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
}
