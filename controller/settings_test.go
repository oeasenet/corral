package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsFromEnv_SeedsFromEnvironment(t *testing.T) {
	s := SettingsFromEnv([]string{
		"RUNNER_COUNT=5",
		"RUNNER_LABELS=docker, oease ",
		"RUNNER_GROUP=prod",
		"RUNNER_IMAGE=ghcr.io/oeasenet/gha-docker-runner/runner:sha-123",
		"RUNNER_EPHEMERAL=true",
		"RUNNER_DOCKER_SOCKET=false",
		"RUNNER_WORK_BASE=/srv/gha",
		"RUNNER_GRACEFUL_STOP_TIMEOUT=120",
		"RUNNER_EXTRA_ENV=ADDITIONAL_PACKAGES=kubectl",
	})
	if s.Count != 5 || s.Labels != "docker,oease" || s.Group != "prod" || s.Image != "ghcr.io/oeasenet/gha-docker-runner/runner:sha-123" {
		t.Errorf("basic fields: %+v", s)
	}
	if !s.Ephemeral || s.DockerSocket || s.WorkBase != "/srv/gha" || s.GracefulStopSeconds != 120 || s.ExtraEnv != "ADDITIONAL_PACKAGES=kubectl" {
		t.Errorf("flags: %+v", s)
	}
}

func TestSettingsFromEnv_Defaults(t *testing.T) {
	s := SettingsFromEnv(nil)
	if s.Count != 2 || s.Image != DefaultRunnerImage || !s.DockerSocket || s.Ephemeral || s.GracefulStopSeconds != 900 {
		t.Errorf("defaults: %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestSettingsValidate(t *testing.T) {
	good := SettingsFromEnv(nil)
	bad := []Settings{
		func() Settings { s := good; s.Count = -1; return s }(),
		func() Settings { s := good; s.Count = 1000; return s }(),
		func() Settings { s := good; s.Image = ""; return s }(),
		func() Settings { s := good; s.GracefulStopSeconds = -5; return s }(),
		func() Settings { s := good; s.WorkBase = "relative/path"; return s }(),
		func() Settings { s := good; s.ExtraEnv = "NOEQUALSSIGN"; return s }(),
		func() Settings { s := good; s.ExtraEnv = "RUNNER_TOKEN=x"; return s }(),
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("case %d should fail validation: %+v", i, s)
		}
	}
}

func TestLoadSettings_CreatesFileFromSeedThenFileWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	seed := SettingsFromEnv([]string{"RUNNER_COUNT=4", "RUNNER_LABELS=docker"})

	store, err := LoadSettings(path, seed)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if store.Get().Count != 4 {
		t.Errorf("seed not applied: %+v", store.Get())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("settings file should be written on first start: %v", err)
	}

	if _, err := store.Update(func(s *Settings) error { s.Count = 7; return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	again, err := LoadSettings(path, SettingsFromEnv([]string{"RUNNER_COUNT=1"}))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.Get().Count != 7 || again.Get().Labels != "docker" {
		t.Errorf("file must win over env once it exists: %+v", again.Get())
	}
}

func TestLoadSettings_RejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"count": "many"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path, SettingsFromEnv(nil)); err == nil {
		t.Fatal("a corrupt settings file must not be silently replaced")
	}
}

func TestSettingsUpdate_ValidatesAndDoesNotPersistBadValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	store, err := LoadSettings(path, SettingsFromEnv([]string{"RUNNER_COUNT=2"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(s *Settings) error { s.Count = -3; return nil }); err == nil {
		t.Fatal("invalid update must be rejected")
	}
	if store.Get().Count != 2 {
		t.Errorf("in-memory settings changed despite rejection: %+v", store.Get())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "-3") {
		t.Errorf("rejected value was persisted: %s", data)
	}
}

func TestGeneration_ChangesWithRunnerShapeButNotCount(t *testing.T) {
	base := SettingsFromEnv(nil)
	same := base
	same.Count = 9
	if base.Generation() != same.Generation() {
		t.Error("count must not change the generation")
	}
	for _, mutate := range []func(*Settings){
		func(s *Settings) { s.Labels = "gpu" },
		func(s *Settings) { s.Group = "g" },
		func(s *Settings) { s.Image = "other:tag" },
		func(s *Settings) { s.Ephemeral = true },
		func(s *Settings) { s.DockerSocket = false },
		func(s *Settings) { s.WorkBase = "/srv/x" },
		func(s *Settings) { s.ExtraEnv = "A=b" },
		func(s *Settings) { s.GracefulStopSeconds = 1 },
	} {
		s := base
		mutate(&s)
		if s.Generation() == base.Generation() {
			t.Errorf("generation unchanged after %+v", s)
		}
	}
	if len(base.Generation()) < 8 {
		t.Errorf("generation should be a reasonably long hash, got %q", base.Generation())
	}
}
