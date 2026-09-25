package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFeatureFlags(t *testing.T) {
	fm := NewFeatureManager(nil)
	if !fm.IsEnabled(FlagEngineV2) {
		t.Fatalf("expected %s to be enabled by default", FlagEngineV2)
	}

	var changedFlag string
	var changedVal bool
	fm.RegisterWatcher(func(flag string, enabled bool) {
		changedFlag = flag
		changedVal = enabled
	})

	fm.Set(FlagEngineV2, false)
	if fm.IsEnabled(FlagEngineV2) {
		t.Fatalf("expected %s to be disabled after Set", FlagEngineV2)
	}
	if changedFlag != FlagEngineV2 || changedVal != false {
		t.Fatalf("watcher did not receive correct event: %s=%v", changedFlag, changedVal)
	}

	snap := fm.AllSnapshot()
	if snap[FlagEngineV2] != false {
		t.Fatalf("snapshot does not reflect updated value")
	}
}

func TestVersionManager_PromoteAndRollback(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ximo-test-vm-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	paths := DefaultPaths()
	paths.BaseDir = tmpDir
	paths.VersionsDir = filepath.Join(tmpDir, "versions")
	paths.CurrentPointerFile = filepath.Join(tmpDir, "current")
	paths.PreviousPointerFile = filepath.Join(tmpDir, "previous")
	paths.CurrentVersionDir = filepath.Join(paths.VersionsDir, "current")
	paths.PreviousVersionDir = filepath.Join(paths.VersionsDir, "previous")

	vm := NewVersionManager(paths)

	metaV1 := &VersionMeta{
		Version:      "v1.0.0",
		HealthStatus: HealthStatusHealthy,
		InstalledAt:  time.Now(),
	}

	v1Source := filepath.Join(tmpDir, "pkg-v1")
	_ = os.MkdirAll(v1Source, 0755)
	if err := vm.PromoteToCurrent(v1Source, metaV1); err != nil {
		t.Fatalf("promote v1 failed: %v", err)
	}

	cur, err := vm.GetCurrentVersion()
	if err != nil || cur.Version != "v1.0.0" {
		t.Fatalf("unexpected current version: %+v, err: %v", cur, err)
	}

	metaV2 := &VersionMeta{
		Version:      "v2.0.0",
		HealthStatus: HealthStatusHealthy,
		InstalledAt:  time.Now(),
	}
	v2Source := filepath.Join(tmpDir, "pkg-v2")
	_ = os.MkdirAll(v2Source, 0755)
	if err := vm.PromoteToCurrent(v2Source, metaV2); err != nil {
		t.Fatalf("promote v2 failed: %v", err)
	}

	cur, err = vm.GetCurrentVersion()
	if err != nil || cur.Version != "v2.0.0" {
		t.Fatalf("current should be v2.0.0, got %+v", cur)
	}
	prev, err := vm.GetPreviousVersion()
	if err != nil || prev.Version != "v1.0.0" {
		t.Fatalf("previous should be v1.0.0, got %+v", prev)
	}

	// 验证回滚
	if err := vm.RollbackToPrevious(); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	cur, err = vm.GetCurrentVersion()
	if err != nil || cur.Version != "v1.0.0" {
		t.Fatalf("after rollback, current should be v1.0.0, got %+v", cur)
	}

	// 验证健康检查钩子
	vm.SetHealthCheckHook(func(ctx context.Context, dir string) error {
		return nil
	})
	if err := vm.VerifyHealth(context.Background()); err != nil {
		t.Fatalf("health check failed: %v", err)
	}
}

func TestConfigLoadAndSave(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ximo-test-cfg-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := NewDefaultConfig()
	cfg.Supervisor.MaxRestartRetries = 99
	cfg.FeatureFlags[FlagSchedulerV2] = false

	if err := cfg.SaveConfig(cfgPath); err != nil {
		t.Fatalf("save config failed: %v", err)
	}

	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config failed: %v", err)
	}
	if loaded.Supervisor.MaxRestartRetries != 99 {
		t.Fatalf("expected 99 retries, got %d", loaded.Supervisor.MaxRestartRetries)
	}
	if loaded.FeatureFlags[FlagSchedulerV2] != false {
		t.Fatalf("expected scheduler.v2 to be false")
	}
}
