package main

import (
	"strings"
	"testing"
	"time"
)

func TestLoadControllerConfig_RequiresPATAndTarget(t *testing.T) {
	if _, err := loadControllerConfig([]string{"GITHUB_OWNER=example-org"}); err == nil || !strings.Contains(err.Error(), "GITHUB_PAT") {
		t.Errorf("missing PAT should be reported, got %v", err)
	}
	if _, err := loadControllerConfig([]string{"GITHUB_PAT=ghp_x"}); err == nil || !strings.Contains(err.Error(), "GITHUB_OWNER") {
		t.Errorf("missing target should be reported, got %v", err)
	}
	if _, err := loadControllerConfig([]string{"GITHUB_PAT=ghp_x", "GITHUB_OWNER=bad name"}); err == nil {
		t.Error("invalid owner should be rejected")
	}
}

func TestLoadControllerConfig_Defaults(t *testing.T) {
	cfg, err := loadControllerConfig([]string{"GITHUB_PAT=ghp_x", "GITHUB_OWNER=example-org"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PAT != "ghp_x" || cfg.Target.String() != "example-org" {
		t.Errorf("basic: %+v", cfg)
	}
	if cfg.Port != "8080" || cfg.DataDir != "/data" || cfg.DockerHost != "unix:///var/run/docker.sock" || cfg.DockerAPIVersion != "1.44" || cfg.FlavorsDir != "/etc/corral/images" {
		t.Errorf("service defaults: %+v", cfg)
	}
	if cfg.NamePrefix != "corral" || cfg.Interval != 30*time.Second || cfg.DockerSocket != "/var/run/docker.sock" {
		t.Errorf("controller defaults: %+v", cfg.ControllerConfig)
	}
	if cfg.GitHubAPI != "https://api.github.com" || cfg.GitHubURL != "https://github.com" || cfg.RegistryAuth != nil || cfg.AdminPassword != "" {
		t.Errorf("github/registry defaults: %+v", cfg)
	}
	if cfg.UpdateInterval != time.Hour {
		t.Errorf("update interval default: %v", cfg.UpdateInterval)
	}
}

func TestLoadControllerConfig_Overrides(t *testing.T) {
	cfg, err := loadControllerConfig([]string{
		"GITHUB_PAT=ghp_x", "GITHUB_OWNER=ignored", "RUNNER_REGISTER_TO=example-org/platform",
		"PORT=9090", "DATA_DIR=/var/lib/gha", "DOCKER_HOST=tcp://docker:2375", "DOCKER_API_VERSION=1.45",
		"RUNNER_NAME_PREFIX=ci", "RECONCILE_INTERVAL=10s", "RUNNER_DOCKER_SOCKET_PATH=/run/docker.sock",
		"REGISTRY_USERNAME=tony", "REGISTRY_PASSWORD=ghp_pull", "ADMIN_PASSWORD=s3cret",
		"GITHUB_API_URL=https://ghe.example.com/api/v3/", "GITHUB_URL=https://ghe.example.com/",
		"UPDATE_CHECK_INTERVAL=15m", "FLAVORS_DIR=/srv/flavors",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Target.String() != "example-org/platform" {
		t.Errorf("RUNNER_REGISTER_TO must win over GITHUB_OWNER, got %s", cfg.Target)
	}
	if cfg.Port != "9090" || cfg.DataDir != "/var/lib/gha" || cfg.DockerHost != "tcp://docker:2375" || cfg.DockerAPIVersion != "1.45" {
		t.Errorf("service overrides: %+v", cfg)
	}
	if cfg.NamePrefix != "ci" || cfg.Interval != 10*time.Second || cfg.DockerSocket != "/run/docker.sock" {
		t.Errorf("controller overrides: %+v", cfg.ControllerConfig)
	}
	if cfg.RegistryAuth == nil || cfg.RegistryAuth.Username != "tony" || cfg.RegistryAuth.Password != "ghp_pull" || cfg.AdminPassword != "s3cret" {
		t.Errorf("registry/admin: %+v", cfg)
	}
	if cfg.GitHubAPI != "https://ghe.example.com/api/v3" || cfg.GitHubURL != "https://ghe.example.com" {
		t.Errorf("GHE urls should be trimmed: %q %q", cfg.GitHubAPI, cfg.GitHubURL)
	}
	if cfg.UpdateInterval != 15*time.Minute || cfg.FlavorsDir != "/srv/flavors" {
		t.Errorf("update interval / flavors dir override: %v %s", cfg.UpdateInterval, cfg.FlavorsDir)
	}
}

func TestLoadControllerConfig_RejectsBadValues(t *testing.T) {
	base := []string{"GITHUB_PAT=ghp_x", "GITHUB_OWNER=example-org"}
	for _, bad := range []string{"RECONCILE_INTERVAL=soon", "RUNNER_NAME_PREFIX=has space", "RUNNER_NAME_PREFIX=", "UPDATE_CHECK_INTERVAL=sometimes", "UPDATE_CHECK_INTERVAL=-1h"} {
		if _, err := loadControllerConfig(append(base, bad)); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestLoadControllerConfig_ZeroUpdateIntervalDisablesChecks(t *testing.T) {
	cfg, err := loadControllerConfig([]string{"GITHUB_PAT=ghp_x", "GITHUB_OWNER=example-org", "UPDATE_CHECK_INTERVAL=0"})
	if err != nil || cfg.UpdateInterval != 0 {
		t.Errorf("zero must disable: %v %v", cfg.UpdateInterval, err)
	}
}
