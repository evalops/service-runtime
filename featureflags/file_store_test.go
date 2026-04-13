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
