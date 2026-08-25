package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// RunnerImageRepo is where CI publishes the runner flavors (tag = runtime).
const RunnerImageRepo = "ghcr.io/oeasenet/corral/runner"

// DefaultRunnerImage is the image of the default (ubuntu) runtime.
const DefaultRunnerImage = RunnerImageRepo + ":ubuntu"

// LegacyRunnerImage is what version-1 settings pointed at; it stays an alias of ubuntu.
const LegacyRunnerImage = RunnerImageRepo + ":latest"

const (
	maxRunnerCount             = 200
	defaultGracefulStopSeconds = 900
)

// Pool names are short because they are embedded in runner names; flavor
// names (images/<name>, image tags) may be longer. scripts/flavors.sh enforces
// the same rule for flavors.
var (
	poolNamePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)
	flavorNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// Pool is one group of identical runners: a runtime environment plus the
// options that shape its containers. Name is the key and never changes.
type Pool struct {
	Name                string `json:"name"`
	Runtime             string `json:"runtime"`
	Image               string `json:"image"`
	Count               int    `json:"count"`
	Labels              string `json:"labels"`
	Group               string `json:"group"`
	Ephemeral           bool   `json:"ephemeral"`
	DockerSocket        bool   `json:"docker_socket"`
	WorkBase            string `json:"work_base"`
	ExtraEnv            string `json:"extra_env"`
	GracefulStopSeconds int    `json:"graceful_stop_seconds"`
}

// EffectiveImage is the image the pool's containers run: the override when
// set, otherwise the CI-built image of the runtime. Empty for a custom
// runtime without an image.
func (p Pool) EffectiveImage() string {
	if img := strings.TrimSpace(p.Image); img != "" {
		return img
	}
	if p.Runtime == "" || p.Runtime == "custom" {
		return ""
	}
	return RunnerImageRepo + ":" + p.Runtime
}

// Validate rejects a pool the controller cannot act on.
func (p Pool) Validate() error {
	if !poolNamePattern.MatchString(p.Name) {
		return fmt.Errorf("pool name %q must be 1-24 lowercase letters, digits or dashes", p.Name)
	}
	if p.Runtime != "custom" && !flavorNamePattern.MatchString(p.Runtime) {
		return fmt.Errorf("pool %s: runtime %q must be a flavor name (lowercase letters, digits, dashes) or \"custom\"", p.Name, p.Runtime)
	}
	if p.EffectiveImage() == "" {
		return fmt.Errorf("pool %s: a custom runtime needs an image", p.Name)
	}
	if p.Count < 0 || p.Count > maxRunnerCount {
		return fmt.Errorf("pool %s: count must be between 0 and %d", p.Name, maxRunnerCount)
	}
	if p.GracefulStopSeconds < 0 {
		return fmt.Errorf("pool %s: graceful stop seconds cannot be negative", p.Name)
	}
	if p.WorkBase != "" && !filepath.IsAbs(p.WorkBase) {
		return fmt.Errorf("pool %s: work base must be an absolute host path", p.Name)
	}
	for _, kv := range p.ExtraEnvList() {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || !envKeyPattern.MatchString(key) {
			return fmt.Errorf("pool %s: extra env entry %q must look like KEY=value", p.Name, kv)
		}
		if reservedEnv[key] {
			return fmt.Errorf("pool %s: extra env %s is managed by the controller", p.Name, key)
		}
	}
	return nil
}

// Generation identifies the shape of a container built from this pool;
// containers with a different generation are replaced. Name and count are
// deliberately excluded.
func (p Pool) Generation() string {
	shape := struct {
		Image, Labels, Group    string
		Ephemeral, DockerSocket bool
		WorkBase, ExtraEnv      string
		GracefulStopSeconds     int
	}{p.EffectiveImage(), normalizeLabels(p.Labels), p.Group, p.Ephemeral, p.DockerSocket, p.WorkBase, p.ExtraEnv, p.GracefulStopSeconds}
	data, _ := json.Marshal(shape)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// ExtraEnvList returns ExtraEnv as KEY=VALUE entries, one per non-empty line.
func (p Pool) ExtraEnvList() []string {
	var out []string
	for _, line := range strings.Split(p.ExtraEnv, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// LabelList returns the configured labels as a slice (nil when empty).
func (p Pool) LabelList() []string {
	labels := normalizeLabels(p.Labels)
	if labels == "" {
		return nil
	}
	return strings.Split(labels, ",")
}

// normalized trims and lower-cases what users type.
func (p Pool) normalized() Pool {
	p.Name = strings.TrimSpace(p.Name)
	p.Runtime = strings.ToLower(strings.TrimSpace(p.Runtime))
	p.Image = strings.TrimSpace(p.Image)
	p.Labels = normalizeLabels(p.Labels)
	p.Group = strings.TrimSpace(p.Group)
	p.WorkBase = strings.TrimSpace(p.WorkBase)
	return p
}
