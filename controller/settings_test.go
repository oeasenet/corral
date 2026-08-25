package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsFromEnv_SeedsFirstPoolFromEnvironment(t *testing.T) {
	s := SettingsFromEnv([]string{
		"RUNNER_RUNTIME=Debian",
		"RUNNER_COUNT=5",
		"RUNNER_LABELS=docker, example ",
		"RUNNER_GROUP=prod",
		"RUNNER_EPHEMERAL=true",
		"RUNNER_DOCKER_SOCKET=false",
		"RUNNER_WORK_BASE=/srv/gha",
		"RUNNER_GRACEFUL_STOP_TIMEOUT=120",
		"RUNNER_EXTRA_ENV=ADDITIONAL_PACKAGES=kubectl",
		"AUTO_UPDATE=false",
	})
	if s.Version != settingsVersion || s.AutoUpdate || len(s.Pools) != 1 {
		t.Fatalf("top level: %+v", s)
	}
	p := s.Pools[0]
	if p.Name != "debian" || p.Runtime != "debian" || p.Image != "" || p.EffectiveImage() != RunnerImageRepo+":debian" {
		t.Errorf("runtime/name: %+v", p)
	}
	if p.Count != 5 || p.Labels != "docker,example" || p.Group != "prod" || !p.Ephemeral || p.DockerSocket || p.WorkBase != "/srv/gha" || p.GracefulStopSeconds != 120 || p.ExtraEnv != "ADDITIONAL_PACKAGES=kubectl" {
		t.Errorf("fields: %+v", p)
	}
}

func TestSettingsFromEnv_DefaultsAndCustomImage(t *testing.T) {
	s := SettingsFromEnv(nil)
	if !s.AutoUpdate || len(s.Pools) != 1 {
		t.Fatalf("defaults: %+v", s)
	}
	p := s.Pools[0]
	if p.Name != "ubuntu" || p.Runtime != "ubuntu" || p.Count != 2 || !p.DockerSocket || p.Ephemeral || p.GracefulStopSeconds != 900 || p.EffectiveImage() != DefaultRunnerImage {
		t.Errorf("default pool: %+v", p)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}

	// An explicit image without a runtime is a custom pool named "default".
	c := SettingsFromEnv([]string{"RUNNER_IMAGE=registry.example.com/runner:1"}).Pools[0]
	if c.Name != "default" || c.Runtime != "custom" || c.EffectiveImage() != "registry.example.com/runner:1" {
		t.Errorf("custom pool: %+v", c)
	}
	// An explicit image with a runtime pins that runtime's pool to the image.
	pinned := SettingsFromEnv([]string{"RUNNER_RUNTIME=ubuntu", "RUNNER_IMAGE=" + RunnerImageRepo + ":ubuntu-2.336.0"}).Pools[0]
	if pinned.Name != "ubuntu" || pinned.Runtime != "ubuntu" || pinned.EffectiveImage() != RunnerImageRepo+":ubuntu-2.336.0" {
		t.Errorf("pinned pool: %+v", pinned)
	}
}

func TestSettingsValidate_AcrossPools(t *testing.T) {
	s := SettingsFromEnv(nil)
	s.SetPool(Pool{Name: "debian", Runtime: "debian", Count: 1})
	if err := s.Validate(); err != nil {
		t.Fatalf("two pools: %v", err)
	}
	dup := s
	dup.Pools = append(append([]Pool(nil), s.Pools...), Pool{Name: "debian", Runtime: "debian"})
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate names must be rejected: %v", err)
	}
	big := SettingsFromEnv(nil)
	big.Pools[0].Count = 150
	big.SetPool(Pool{Name: "b", Runtime: "debian", Count: 100})
	if err := big.Validate(); err == nil || !strings.Contains(err.Error(), "total") {
		t.Errorf("total count over the limit must be rejected: %v", err)
	}
	none := Settings{Version: settingsVersion}
	if err := none.Validate(); err != nil {
		t.Errorf("zero pools is a valid (idle) configuration: %v", err)
	}
}

func TestSettings_PoolHelpers(t *testing.T) {
	s := SettingsFromEnv(nil)
	s.SetPool(Pool{Name: "debian", Runtime: "debian", Count: 3})
	s.SetPool(Pool{Name: "ubuntu", Runtime: "ubuntu", Count: 4, Image: RunnerImageRepo + ":ubuntu"})
	if len(s.Pools) != 2 || s.Pools[0].Name != "ubuntu" || s.Pools[0].Count != 4 || s.Pools[1].Name != "debian" {
		t.Errorf("SetPool must replace in place and append new: %+v", s.Pools)
	}
	if p, ok := s.Pool("debian"); !ok || p.Count != 3 {
		t.Errorf("Pool lookup: %+v %v", p, ok)
	}
	if _, ok := s.Pool("nope"); ok {
		t.Error("unknown pool must not be found")
	}
	if s.TotalCount() != 7 {
		t.Errorf("TotalCount: %d", s.TotalCount())
	}
	if imgs := s.DistinctImages(); len(imgs) != 2 || imgs[0] != RunnerImageRepo+":debian" || imgs[1] != RunnerImageRepo+":ubuntu" {
		t.Errorf("DistinctImages: %v", imgs)
	}
	if !s.RemovePool("debian") || s.RemovePool("debian") || len(s.Pools) != 1 {
		t.Errorf("RemovePool: %+v", s.Pools)
	}
}

func TestLoadSettings_CreatesFileFromSeedThenFileWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := LoadSettings(path, SettingsFromEnv([]string{"RUNNER_COUNT=4", "RUNNER_LABELS=docker"}))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if store.Get().Pools[0].Count != 4 {
		t.Errorf("seed not applied: %+v", store.Get())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("settings file should be written on first start: %v", err)
	}
	if _, err := store.Update(func(s *Settings) error { s.Pools[0].Count = 7; return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	again, err := LoadSettings(path, SettingsFromEnv([]string{"RUNNER_COUNT=1"}))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if p := again.Get().Pools[0]; p.Count != 7 || p.Labels != "docker" {
		t.Errorf("file must win over env once it exists: %+v", p)
	}
}

func TestLoadSettings_MigratesVersion1(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	v1 := `{"count":3,"labels":"docker,example","group":"prod","image":"ghcr.io/oeasenet/corral/runner:latest","ephemeral":false,"docker_socket":true,"work_base":"/srv/gha","extra_env":"A=1","graceful_stop_seconds":600}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := LoadSettings(path, SettingsFromEnv(nil))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	s := store.Get()
	if s.Version != settingsVersion || !s.AutoUpdate || len(s.Pools) != 1 {
		t.Fatalf("migrated: %+v", s)
	}
	p := s.Pools[0]
	if p.Name != "ubuntu" || p.Runtime != "ubuntu" || p.Image != "" || p.Count != 3 || p.Labels != "docker,example" || p.Group != "prod" || p.WorkBase != "/srv/gha" || p.ExtraEnv != "A=1" || p.GracefulStopSeconds != 600 {
		t.Errorf("migrated pool: %+v", p)
	}
	backup, err := os.ReadFile(path + ".v1")
	if err != nil || string(backup) != v1 {
		t.Errorf("original file must be kept as .v1: %v %q", err, backup)
	}
	data, _ := os.ReadFile(path)
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk["pools"] == nil {
		t.Errorf("migrated file must be written in v2 format: %s", data)
	}

	// A custom image becomes a custom pool named "default".
	custom := filepath.Join(dir, "custom.json")
	_ = os.WriteFile(custom, []byte(`{"count":1,"image":"registry.example.com/r:1","docker_socket":true,"graceful_stop_seconds":900}`), 0o600)
	cs, err := LoadSettings(custom, SettingsFromEnv(nil))
	if err != nil {
		t.Fatalf("custom migration: %v", err)
	}
	if cp := cs.Get().Pools[0]; cp.Name != "default" || cp.Runtime != "custom" || cp.Image != "registry.example.com/r:1" {
		t.Errorf("custom migrated pool: %+v", cp)
	}
}

func TestLoadSettings_RejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(path, []byte(`{"pools": "many"`), 0o600)
	if _, err := LoadSettings(path, SettingsFromEnv(nil)); err == nil {
		t.Fatal("a corrupt settings file must not be silently replaced")
	}
	_ = os.WriteFile(path, []byte(`{"version":2,"pools":[{"name":"BAD NAME","runtime":"ubuntu"}]}`), 0o600)
	if _, err := LoadSettings(path, SettingsFromEnv(nil)); err == nil {
		t.Fatal("an invalid pool in the file must be reported, not ignored")
	}
}

func TestSettingsUpdate_ValidatesAndDoesNotPersistBadValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := LoadSettings(path, SettingsFromEnv([]string{"RUNNER_COUNT=2"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(s *Settings) error { s.Pools[0].Count = -3; return nil }); err == nil {
		t.Fatal("invalid update must be rejected")
	}
	if store.Get().Pools[0].Count != 2 {
		t.Errorf("in-memory settings changed despite rejection: %+v", store.Get())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "-3") {
		t.Errorf("rejected value was persisted: %s", data)
	}
	// Update normalizes what users type.
	if _, err := store.Update(func(s *Settings) error {
		s.SetPool(Pool{Name: " deb ", Runtime: "Debian", Labels: " x , y"})
		return nil
	}); err != nil {
		t.Fatalf("normalized update: %v", err)
	}
	if p, ok := store.Get().Pool("deb"); !ok || p.Runtime != "debian" || p.Labels != "x,y" {
		t.Errorf("normalization: %+v", store.Get().Pools)
	}
}
