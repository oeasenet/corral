package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// DefaultRunnerImage is used unless RUNNER_IMAGE / the UI say otherwise.
const DefaultRunnerImage = "ghcr.io/oeasenet/gha-docker-runner/runner:latest"

const maxRunnerCount = 200

// Settings is the runtime-editable configuration. Everything except Count
// shapes the runner containers and therefore participates in Generation().
type Settings struct {
	Count               int    `json:"count"`
	Labels              string `json:"labels"`
	Group               string `json:"group"`
	Image               string `json:"image"`
	Ephemeral           bool   `json:"ephemeral"`
	DockerSocket        bool   `json:"docker_socket"`
	WorkBase            string `json:"work_base"`
	ExtraEnv            string `json:"extra_env"`
	GracefulStopSeconds int    `json:"graceful_stop_seconds"`
}

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv are set by the controller itself and cannot be overridden via ExtraEnv.
var reservedEnv = map[string]bool{
	"RUNNER_TOKEN": true, "RUNNER_NAME": true, "RUNNER_REGISTER_TO": true, "RUNNER_LABELS": true,
	"RUNNER_GROUP": true, "RUNNER_EPHEMERAL": true, "RUNNER_DISABLE_UPDATE": true,
	"RUNNER_GRACEFUL_STOP_TIMEOUT": true, "RUNNER_WORKDIR": true, "GITHUB_URL": true,
}

func envLookup(environ []string) func(string) string {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = strings.TrimSpace(v)
		}
	}
	return func(k string) string { return m[k] }
}

func parseBool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func normalizeLabels(s string) string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, ",")
}

// SettingsFromEnv builds the initial settings from RUNNER_* variables.
func SettingsFromEnv(environ []string) Settings {
	get := envLookup(environ)
	s := Settings{
		Count:               2,
		Image:               DefaultRunnerImage,
		DockerSocket:        true,
		GracefulStopSeconds: 900,
	}
	if n, err := strconv.Atoi(get("RUNNER_COUNT")); err == nil {
		s.Count = n
	}
	s.Labels = normalizeLabels(get("RUNNER_LABELS"))
	s.Group = get("RUNNER_GROUP")
	if v := get("RUNNER_IMAGE"); v != "" {
		s.Image = v
	}
	s.Ephemeral = parseBool(get("RUNNER_EPHEMERAL"), false)
	s.DockerSocket = parseBool(get("RUNNER_DOCKER_SOCKET"), true)
	s.WorkBase = get("RUNNER_WORK_BASE")
	if n, err := strconv.Atoi(get("RUNNER_GRACEFUL_STOP_TIMEOUT")); err == nil {
		s.GracefulStopSeconds = n
	}
	s.ExtraEnv = strings.ReplaceAll(get("RUNNER_EXTRA_ENV"), ";", "\n")
	return s
}

// ExtraEnvList returns ExtraEnv as KEY=VALUE entries, one per non-empty line.
func (s Settings) ExtraEnvList() []string {
	var out []string
	for _, line := range strings.Split(s.ExtraEnv, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// LabelList returns the configured labels as a slice.
func (s Settings) LabelList() []string {
	if s.Labels == "" {
		return nil
	}
	return strings.Split(s.Labels, ",")
}

// Validate rejects settings the controller cannot act on.
func (s Settings) Validate() error {
	if s.Count < 0 || s.Count > maxRunnerCount {
		return fmt.Errorf("count must be between 0 and %d", maxRunnerCount)
	}
	if strings.TrimSpace(s.Image) == "" {
		return errors.New("image is required")
	}
	if s.GracefulStopSeconds < 0 {
		return errors.New("graceful stop seconds cannot be negative")
	}
	if s.WorkBase != "" && !filepath.IsAbs(s.WorkBase) {
		return errors.New("work base must be an absolute host path")
	}
	for _, kv := range s.ExtraEnvList() {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || !envKeyPattern.MatchString(key) {
			return fmt.Errorf("extra env entry %q must look like KEY=value", kv)
		}
		if reservedEnv[key] {
			return fmt.Errorf("extra env %s is managed by the controller", key)
		}
	}
	return nil
}

// Generation identifies the shape of a runner container built from these
// settings; containers with a different generation are replaced.
func (s Settings) Generation() string {
	shape := s
	shape.Count = 0
	shape.Labels = normalizeLabels(shape.Labels)
	data, _ := json.Marshal(shape)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// SettingsStore persists settings to a JSON file.
type SettingsStore struct {
	path    string
	mu      sync.RWMutex
	current Settings
}

// LoadSettings reads the settings file, or creates it from seed when absent.
func LoadSettings(path string, seed Settings) (*SettingsStore, error) {
	store := &SettingsStore{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := seed.Validate(); err != nil {
			return nil, fmt.Errorf("initial settings from environment: %w", err)
		}
		store.current = seed
		if err := store.save(seed); err != nil {
			return nil, err
		}
		return store, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w (fix or delete the file to start over)", path, err)
	}
	s.Labels = normalizeLabels(s.Labels)
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	store.current = s
	return store, nil
}

// Get returns the current settings.
func (st *SettingsStore) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.current
}

// Update applies fn to a copy, validates it and persists it atomically.
func (st *SettingsStore) Update(fn func(*Settings) error) (Settings, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.current
	if err := fn(&next); err != nil {
		return st.current, err
	}
	next.Labels = normalizeLabels(next.Labels)
	if err := next.Validate(); err != nil {
		return st.current, err
	}
	if err := st.save(next); err != nil {
		return st.current, err
	}
	st.current = next
	return next, nil
}

func (st *SettingsStore) save(s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return fmt.Errorf("commit settings: %w", err)
	}
	return nil
}
