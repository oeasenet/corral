package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const settingsVersion = 2

// Settings is the runtime-editable configuration: the pools of runners and
// whether images are refreshed automatically.
type Settings struct {
	Version    int    `json:"version"`
	AutoUpdate bool   `json:"auto_update"`
	Pools      []Pool `json:"pools"`
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

// SettingsFromEnv builds the initial settings (one pool) from RUNNER_* variables.
func SettingsFromEnv(environ []string) Settings {
	get := envLookup(environ)
	p := Pool{Runtime: "ubuntu", Count: 2, DockerSocket: true, GracefulStopSeconds: defaultGracefulStopSeconds}
	runtime := strings.ToLower(strings.TrimSpace(get("RUNNER_RUNTIME")))
	p.Image = get("RUNNER_IMAGE")
	switch {
	case runtime != "":
		p.Runtime = runtime
	case p.Image != "":
		p.Runtime = "custom" // an image without a runtime is a custom pool
	}
	if n, err := strconv.Atoi(get("RUNNER_COUNT")); err == nil {
		p.Count = n
	}
	p.Labels = normalizeLabels(get("RUNNER_LABELS"))
	p.Group = get("RUNNER_GROUP")
	p.Ephemeral = parseBool(get("RUNNER_EPHEMERAL"), false)
	p.DockerSocket = parseBool(get("RUNNER_DOCKER_SOCKET"), true)
	p.WorkBase = get("RUNNER_WORK_BASE")
	if n, err := strconv.Atoi(get("RUNNER_GRACEFUL_STOP_TIMEOUT")); err == nil {
		p.GracefulStopSeconds = n
	}
	p.ExtraEnv = strings.ReplaceAll(get("RUNNER_EXTRA_ENV"), ";", "\n")
	p.Name = p.Runtime
	if p.Runtime == "custom" {
		p.Name = "default"
	}
	return Settings{Version: settingsVersion, AutoUpdate: parseBool(get("AUTO_UPDATE"), true), Pools: []Pool{p}}
}

// Validate rejects settings the controller cannot act on.
func (s Settings) Validate() error {
	seen := map[string]bool{}
	total := 0
	for _, p := range s.Pools {
		if err := p.Validate(); err != nil {
			return err
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		seen[p.Name] = true
		total += p.Count
	}
	if total > maxRunnerCount {
		return fmt.Errorf("total runner count %d exceeds the limit of %d", total, maxRunnerCount)
	}
	return nil
}

// Pool returns the pool with that name.
func (s Settings) Pool(name string) (Pool, bool) {
	for _, p := range s.Pools {
		if p.Name == name {
			return p, true
		}
	}
	return Pool{}, false
}

// TotalCount is the desired number of runners across all pools.
func (s Settings) TotalCount() int {
	n := 0
	for _, p := range s.Pools {
		n += p.Count
	}
	return n
}

// DistinctImages lists every effective image once, sorted.
func (s Settings) DistinctImages() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range s.Pools {
		if img := p.EffectiveImage(); img != "" && !seen[img] {
			seen[img] = true
			out = append(out, img)
		}
	}
	sort.Strings(out)
	return out
}

// SetPool replaces the pool with the same name in place, or appends it.
func (s *Settings) SetPool(p Pool) {
	for i := range s.Pools {
		if s.Pools[i].Name == p.Name {
			s.Pools[i] = p
			return
		}
	}
	s.Pools = append(s.Pools, p)
}

// RemovePool deletes a pool by name; false when there is no such pool.
func (s *Settings) RemovePool(name string) bool {
	for i := range s.Pools {
		if s.Pools[i].Name == name {
			s.Pools = append(s.Pools[:i:i], s.Pools[i+1:]...)
			return true
		}
	}
	return false
}

func (s Settings) normalized() Settings {
	s.Version = settingsVersion
	pools := make([]Pool, len(s.Pools))
	for i, p := range s.Pools {
		pools[i] = p.normalized()
	}
	s.Pools = pools
	return s
}

// legacySettings is the version-1 file layout (one implicit pool).
type legacySettings struct {
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

func migrateLegacy(l legacySettings) Settings {
	p := Pool{
		Count: l.Count, Labels: l.Labels, Group: l.Group, Ephemeral: l.Ephemeral, DockerSocket: l.DockerSocket,
		WorkBase: l.WorkBase, ExtraEnv: l.ExtraEnv, GracefulStopSeconds: l.GracefulStopSeconds,
	}
	if l.Image == "" || l.Image == DefaultRunnerImage || l.Image == LegacyRunnerImage {
		p.Name, p.Runtime = "ubuntu", "ubuntu"
	} else {
		p.Name, p.Runtime, p.Image = "default", "custom", l.Image
	}
	return Settings{Version: settingsVersion, AutoUpdate: true, Pools: []Pool{p}}
}

// SettingsStore persists settings to a JSON file.
type SettingsStore struct {
	path    string
	mu      sync.RWMutex
	current Settings
}

// LoadSettings reads the settings file, creating it from seed when absent and
// migrating a version-1 file (the original is kept as <path>.v1).
func LoadSettings(path string, seed Settings) (*SettingsStore, error) {
	store := &SettingsStore{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		seed = seed.normalized()
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
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("parse %s: %w (fix or delete the file to start over)", path, err)
	}
	var s Settings
	if _, isV2 := probe["pools"]; isV2 {
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("parse %s: %w (fix or delete the file to start over)", path, err)
		}
		s = s.normalized()
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		store.current = s
		return store, nil
	}
	var legacy legacySettings
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("parse %s: %w (fix or delete the file to start over)", path, err)
	}
	s = migrateLegacy(legacy).normalized()
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s (migrated from version 1): %w", path, err)
	}
	if err := os.WriteFile(path+".v1", data, 0o600); err != nil {
		return nil, fmt.Errorf("back up version-1 settings: %w", err)
	}
	store.current = s
	if err := store.save(s); err != nil {
		return nil, err
	}
	return store, nil
}

// Get returns the current settings (the pool slice is a copy).
func (st *SettingsStore) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s := st.current
	s.Pools = append([]Pool(nil), st.current.Pools...)
	return s
}

// Update applies fn to a copy, normalizes and validates it, and persists it atomically.
func (st *SettingsStore) Update(fn func(*Settings) error) (Settings, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.current
	next.Pools = append([]Pool(nil), st.current.Pools...)
	if err := fn(&next); err != nil {
		return st.current, err
	}
	next = next.normalized()
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
