package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// controllerEnv is the environment-only configuration (secrets and wiring).
type controllerEnv struct {
	ControllerConfig
	PAT              string
	AdminPassword    string
	Port             string
	DataDir          string
	DockerHost       string
	DockerAPIVersion string
	GitHubAPI        string
	FlavorsDir       string
}

var validPrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func loadControllerConfig(environ []string) (controllerEnv, error) {
	values := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			values[k] = strings.TrimSpace(v)
		}
	}
	get := func(k string) string { return values[k] }
	present := func(k string) bool { _, ok := values[k]; return ok }

	cfg := controllerEnv{
		ControllerConfig: ControllerConfig{
			NamePrefix:     "corral",
			GitHubURL:      "https://github.com",
			DockerSocket:   "/var/run/docker.sock",
			Interval:       30 * time.Second,
			UpdateInterval: time.Hour,
		},
		Port:             "8080",
		DataDir:          "/data",
		DockerHost:       "unix:///var/run/docker.sock",
		DockerAPIVersion: "1.44",
		GitHubAPI:        "https://api.github.com",
		FlavorsDir:       "/etc/corral/images",
	}

	cfg.PAT = get("GITHUB_PAT")
	if cfg.PAT == "" {
		return cfg, errors.New("GITHUB_PAT is required (a PAT that can manage self-hosted runners)")
	}
	targetStr := get("RUNNER_REGISTER_TO")
	if targetStr == "" {
		targetStr = get("GITHUB_OWNER")
	}
	if targetStr == "" {
		return cfg, errors.New("GITHUB_OWNER (organization) or RUNNER_REGISTER_TO (owner/repo) is required")
	}
	target, err := ParseTarget(targetStr)
	if err != nil {
		return cfg, err
	}
	cfg.Target = target

	if v := get("PORT"); v != "" {
		cfg.Port = v
	}
	if v := get("DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := get("FLAVORS_DIR"); v != "" {
		cfg.FlavorsDir = v
	}
	if v := get("DOCKER_HOST"); v != "" {
		cfg.DockerHost = v
	}
	if v := get("DOCKER_API_VERSION"); v != "" {
		cfg.DockerAPIVersion = strings.TrimPrefix(v, "v")
	}
	if present("RUNNER_NAME_PREFIX") {
		cfg.NamePrefix = get("RUNNER_NAME_PREFIX")
	}
	if !validPrefix.MatchString(cfg.NamePrefix) {
		return cfg, fmt.Errorf("RUNNER_NAME_PREFIX %q must be lowercase letters, digits and dashes", cfg.NamePrefix)
	}
	if v := get("RECONCILE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("RECONCILE_INTERVAL %q must be a duration like 30s", v)
		}
		cfg.Interval = d
	}
	if v := get("UPDATE_CHECK_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return cfg, fmt.Errorf("UPDATE_CHECK_INTERVAL %q must be a duration like 1h (0 disables automatic image checks)", v)
		}
		cfg.UpdateInterval = d
	}
	if v := get("RUNNER_DOCKER_SOCKET_PATH"); v != "" {
		cfg.DockerSocket = v
	}
	if u := get("REGISTRY_USERNAME"); u != "" {
		cfg.RegistryAuth = &RegistryAuth{Username: u, Password: get("REGISTRY_PASSWORD")}
	}
	cfg.AdminPassword = get("ADMIN_PASSWORD")
	if v := get("GITHUB_API_URL"); v != "" {
		cfg.GitHubAPI = strings.TrimRight(v, "/")
	}
	if v := get("GITHUB_URL"); v != "" {
		cfg.GitHubURL = strings.TrimRight(v, "/")
	}
	return cfg, nil
}
