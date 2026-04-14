package featureflags

import (
	"fmt"
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

func TestFileStoreEnabledForBlankSubjectRequiresFullRollout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.json")
	const gradualFlagKey = "platform.kill_switches.prompts.resolve_api"
	const fullFlagKey = "platform.kill_switches.prompts.render"
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: gradualFlagKey, Enabled: true, RolloutPercent: 10},
			{Key: fullFlagKey, Enabled: true, RolloutPercent: 100},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	if store.EnabledFor(gradualFlagKey, "") {
		t.Fatalf("expected blank subject to stay outside gradual rollout")
	}
	if !store.EnabledFor(fullFlagKey, "") {
		t.Fatalf("expected blank subject to remain enabled for full rollout")
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

func TestFileStoreEnabledForZeroRolloutPercentMeansFullRollout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.json")
	const flagKey = "platform.kill_switches.llm_gateway.inference"
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: flagKey, Enabled: true, RolloutPercent: 0},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Proto3 zero-value (unset) should behave as 100% rollout.
	for _, subject := range []string{"org-alpha", "org-bravo", "org-charlie", "org-delta", "org-echo", "tenant-1", "tenant-2"} {
		if !store.EnabledFor(flagKey, subject) {
			t.Fatalf("expected rollout_percent=0 (unset) to enable flag for all subjects, but %q was excluded", subject)
		}
	}
}

func TestFileStoreEnabledForOnePercentRollout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.json")
	const flagKey = "llm_gateway.gradual_feature"
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: flagKey, Enabled: true, RolloutPercent: 1},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	enabled := 0
	total := 1000
	for i := range total {
		subject := fmt.Sprintf("org-%04d", i)
		if store.EnabledFor(flagKey, subject) {
			enabled++
		}
	}

	// With 1% rollout over 1000 subjects, expect roughly 10 enabled (1%).
	// Allow a generous range: 1-30 to avoid flaky tests.
	if enabled == 0 {
		t.Fatal("expected at least one subject to be inside 1% rollout")
	}
	if enabled > 30 {
		t.Fatalf("expected ~1%% of subjects enabled, got %d/%d (%d%%)", enabled, total, enabled*100/total)
	}
}

func TestFileStoreEnabledForDisabledFlagIgnoresRollout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.json")
	const flagKey = "platform.disabled_with_rollout"
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: flagKey, Enabled: false, RolloutPercent: 50},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	for _, subject := range []string{"org-alpha", "org-bravo", "org-charlie", "org-delta", "org-echo"} {
		if store.EnabledFor(flagKey, subject) {
			t.Fatalf("expected disabled flag to return false for all subjects, but %q was enabled", subject)
		}
	}
}

func TestFileStoreEnabledForHundredPercentRollout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.json")
	const flagKey = "platform.full_rollout"
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: flagKey, Enabled: true, RolloutPercent: 100},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	for _, subject := range []string{"org-alpha", "org-bravo", "org-charlie", "org-delta", "org-echo", "tenant-1", "tenant-2"} {
		if !store.EnabledFor(flagKey, subject) {
			t.Fatalf("expected rollout_percent=100 to enable flag for all subjects, but %q was excluded", subject)
		}
	}
}

func TestFileStoreEnabledForOverHundredPercentClamps(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.json")
	const flagKey = "platform.overclamped"
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: flagKey, Enabled: true, RolloutPercent: 150},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	for _, subject := range []string{"org-alpha", "org-bravo", "org-charlie", "org-delta", "org-echo"} {
		if !store.EnabledFor(flagKey, subject) {
			t.Fatalf("expected rollout_percent=150 (clamped to 100) to enable flag for all subjects, but %q was excluded", subject)
		}
	}
}

func TestFileStoreSnapshotReturnsClonedCopy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.json")
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: "platform.kill_switches.llm_gateway.inference", Enabled: true},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	snap := store.Snapshot()
	if len(snap.GetFlags()) == 0 {
		t.Fatalf("expected snapshot to contain flags")
	}

	// Mutate the returned snapshot.
	snap.Flags[0].Enabled = false
	snap.Flags[0].Key = "mutated.key"

	// The store's internal state should be unaffected.
	if !store.Enabled("platform.kill_switches.llm_gateway.inference") {
		t.Fatalf("expected store to be unaffected by mutations to returned snapshot")
	}
	if store.Enabled("mutated.key") {
		t.Fatalf("expected mutated key to not appear in store")
	}
}

func TestFileStoreHasExplicitRollout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.json")
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: "simple_flag", Enabled: true, RolloutPercent: 0},
			{Key: "gradual_flag", Enabled: true, RolloutPercent: 30},
		},
	})

	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	if store.HasExplicitRollout("simple_flag") {
		t.Fatalf("expected simple_flag (rollout_percent=0) to not have explicit rollout")
	}
	if !store.HasExplicitRollout("gradual_flag") {
		t.Fatalf("expected gradual_flag (rollout_percent=30) to have explicit rollout")
	}
	if store.HasExplicitRollout("missing_flag") {
		t.Fatalf("expected missing flag to not have explicit rollout")
	}
}

func TestFileStoreHasExplicitRolloutIgnoresEnabledState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "flags.json")
	writeSnapshot(t, path, &configv1.FeatureFlagSnapshot{
		SchemaVersion: 1,
		Flags: []*configv1.FeatureFlag{
			{Key: "disabled.with.rollout", Enabled: false, RolloutPercent: 50},
		},
	})
	store, err := NewFileStore(path, Options{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if !store.HasExplicitRollout("disabled.with.rollout") {
		t.Fatal("expected HasExplicitRollout to return true for disabled flag with rollout_percent > 0")
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
