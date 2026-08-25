package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// In-memory fakes
// ---------------------------------------------------------------------------

type stopCall struct {
	id      string
	timeout time.Duration
}

type fakeDockerAPI struct {
	mu          sync.Mutex
	containers  map[string]*Container
	specs       map[string]ContainerSpec
	imageIDs    map[string]string
	imageLabels map[string]map[string]string
	pulled      []string
	pullAuth    *RegistryAuth
	stops       []stopCall
	removed     []string
	started     []string
	nextID      int
	maxAlive    int
	stopBlock   chan struct{}
	tokenErr    error
}

func newFakeDockerAPI() *fakeDockerAPI {
	return &fakeDockerAPI{
		containers:  map[string]*Container{},
		specs:       map[string]ContainerSpec{},
		imageIDs:    map[string]string{DefaultRunnerImage: "sha256:current"},
		imageLabels: map[string]map[string]string{},
	}
}

func (f *fakeDockerAPI) add(name, state, health, generation, imageID string, created time.Time) string {
	return f.addInPool("", name, state, health, generation, imageID, created)
}

func (f *fakeDockerAPI) addInPool(pool, name, state, health, generation, imageID string, created time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("id-%d", f.nextID)
	labels := map[string]string{labelManaged: "true", labelName: name, labelGeneration: generation}
	if pool != "" {
		labels[labelPool] = pool
	}
	f.containers[id] = &Container{
		ID: id, Name: name, State: state, Health: health, ImageID: imageID, Image: DefaultRunnerImage, Created: created, Labels: labels,
	}
	return id
}

func (f *fakeDockerAPI) alive() int {
	n := 0
	for _, c := range f.containers {
		if c.State == "running" || c.State == "created" || c.State == "restarting" {
			n++
		}
	}
	return n
}

func (f *fakeDockerAPI) ListContainers(_ context.Context, label string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, val, _ := strings.Cut(label, "=")
	var out []Container
	for _, c := range f.containers {
		if c.Labels[key] == val {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *fakeDockerAPI) InspectContainer(_ context.Context, id string) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[id]; ok {
		return *c, nil
	}
	return Container{}, &DockerError{Status: 404, Message: "no such container"}
}

func (f *fakeDockerAPI) CreateContainer(_ context.Context, spec ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.Name == spec.Name {
			return "", &DockerError{Status: 409, Message: "name already in use"}
		}
	}
	f.nextID++
	id := fmt.Sprintf("id-%d", f.nextID)
	f.containers[id] = &Container{ID: id, Name: spec.Name, State: "created", Image: spec.Image, ImageID: f.imageIDs[spec.Image], Labels: spec.Labels, Created: time.Now()}
	f.specs[id] = spec
	if n := f.alive(); n > f.maxAlive {
		f.maxAlive = n
	}
	return id, nil
}

func (f *fakeDockerAPI) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return &DockerError{Status: 404, Message: "no such container"}
	}
	c.State = "running"
	f.started = append(f.started, id)
	return nil
}

func (f *fakeDockerAPI) StopContainer(_ context.Context, id string, timeout time.Duration) error {
	f.mu.Lock()
	block := f.stopBlock
	f.stops = append(f.stops, stopCall{id, timeout})
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[id]; ok {
		c.State = "exited"
	}
	return nil
}

func (f *fakeDockerAPI) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, id)
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeDockerAPI) PullImage(_ context.Context, ref string, auth *RegistryAuth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	f.pullAuth = auth
	f.imageIDs[ref] = "sha256:pulled"
	return nil
}

func (f *fakeDockerAPI) InspectImage(_ context.Context, ref string) (ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ImageInfo{ID: f.imageIDs[ref], Labels: f.imageLabels[ref]}, nil
}

func (f *fakeDockerAPI) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stops)
}

func (f *fakeDockerAPI) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.containers {
		out = append(out, c.Name)
	}
	return out
}

func (f *fakeDockerAPI) byName(name string) *Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.Name == name {
			cp := *c
			return &cp
		}
	}
	return nil
}

type fakeGitHubAPI struct {
	mu        sync.Mutex
	runners   []Runner
	tokens    int
	tokenErr  error
	deleted   []int64
	listErr   error
	latest    string
	latestErr error
}

func (g *fakeGitHubAPI) RegistrationToken(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tokenErr != nil {
		return "", g.tokenErr
	}
	g.tokens++
	return fmt.Sprintf("TOKEN-%d", g.tokens), nil
}

func (g *fakeGitHubAPI) ListRunners(context.Context) ([]Runner, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.listErr != nil {
		return nil, g.listErr
	}
	return append([]Runner(nil), g.runners...), nil
}

func (g *fakeGitHubAPI) DeleteRunner(_ context.Context, id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deleted = append(g.deleted, id)
	var keep []Runner
	for _, r := range g.runners {
		if r.ID != id {
			keep = append(keep, r)
		}
	}
	g.runners = keep
	return nil
}

func (g *fakeGitHubAPI) LatestRunnerVersion(context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.latestErr != nil {
		return "", g.latestErr
	}
	if g.latest == "" {
		return "2.336.0", nil
	}
	return g.latest, nil
}

func (g *fakeGitHubAPI) deletedIDs() []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int64(nil), g.deleted...)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestReconciler(t *testing.T, settings Settings) (*Reconciler, *fakeDockerAPI, *fakeGitHubAPI) {
	t.Helper()
	store, err := LoadSettings(filepath.Join(t.TempDir(), "settings.json"), settings)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	d := newFakeDockerAPI()
	g := &fakeGitHubAPI{}
	r := NewReconciler(d, g, store, ControllerConfig{
		Target:       Target{Owner: "example-org"},
		NamePrefix:   "corral",
		GitHubURL:    "https://github.com",
		RegistryAuth: &RegistryAuth{Username: "u", Password: "p"},
		DockerSocket: "/var/run/docker.sock",
		Flavors: []Flavor{
			{Name: "ubuntu", Label: "Ubuntu 26.04", Base: "ubuntu:26.04", Default: true},
			{Name: "debian", Label: "Debian 13", Base: "debian:13"},
			{Name: "minideb", Label: "minideb (Debian 13)", Base: "bitnami/minideb:trixie"},
		},
	}, NewEventLog(50))
	return r, d, g
}

func baseSettings(count int) Settings {
	s := SettingsFromEnv(nil)
	s.Pools[0].Count = count
	s.Pools[0].Labels = "docker"
	s.Pools[0].GracefulStopSeconds = 300
	return s
}

// gen is the generation of the first pool of s.
func gen(s Settings) string { return s.Pools[0].Generation() }

func count(r *Reconciler, pool string) int {
	p, _ := r.settings.Get().Pool(pool)
	return p.Count
}

func reconcile(t *testing.T, r *Reconciler) {
	t.Helper()
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestReconcile_CreatesDesiredRunnersWithFreshTokens(t *testing.T) {
	r, d, _ := newTestReconciler(t, baseSettings(2))
	reconcile(t, r)

	if len(d.specs) != 2 || len(d.started) != 2 {
		t.Fatalf("expected 2 created+started containers, got specs=%d started=%d", len(d.specs), len(d.started))
	}
	seenTokens := map[string]bool{}
	for _, spec := range d.specs {
		if !strings.HasPrefix(spec.Name, "corral-ubuntu-") || len(spec.Name) != len("corral-ubuntu-")+6 {
			t.Errorf("name should be corral-<pool>-<6 hex>, got %q", spec.Name)
		}
		if spec.Hostname != spec.Name {
			t.Errorf("hostname must equal the runner name: %q vs %q", spec.Hostname, spec.Name)
		}
		if spec.Image != DefaultRunnerImage {
			t.Errorf("image: %q", spec.Image)
		}
		tok, _ := envValue(spec.Env, "RUNNER_TOKEN")
		if !strings.HasPrefix(tok, "TOKEN-") || seenTokens[tok] {
			t.Errorf("each runner needs its own fresh token, got %q", tok)
		}
		seenTokens[tok] = true
		for key, want := range map[string]string{
			"RUNNER_REGISTER_TO": "example-org", "RUNNER_NAME": spec.Name, "RUNNER_LABELS": "docker",
			"RUNNER_EPHEMERAL": "false", "RUNNER_DISABLE_UPDATE": "false", "RUNNER_GRACEFUL_STOP_TIMEOUT": "300",
		} {
			if got, _ := envValue(spec.Env, key); got != want {
				t.Errorf("env %s: got %q, want %q", key, got, want)
			}
		}
		if spec.Labels[labelManaged] != "true" || spec.Labels[labelName] != spec.Name || spec.Labels[labelGeneration] != gen(baseSettings(2)) || spec.Labels[labelPool] != "ubuntu" {
			t.Errorf("labels: %v", spec.Labels)
		}
		if spec.RestartPolicy != "unless-stopped" {
			t.Errorf("persistent runners must restart automatically, got %q", spec.RestartPolicy)
		}
		if spec.StopTimeout != 360 {
			t.Errorf("stop timeout should be graceful+60 = 360, got %d", spec.StopTimeout)
		}
		if len(spec.Binds) != 1 || spec.Binds[0] != "/var/run/docker.sock:/var/run/docker.sock" {
			t.Errorf("binds: %v", spec.Binds)
		}
	}

	// A second pass is a no-op.
	reconcile(t, r)
	if len(d.specs) != 2 {
		t.Errorf("second reconcile created more containers: %d", len(d.specs))
	}
}

func TestBuildSpec_EphemeralWorkBaseExtraEnvAndNoSocket(t *testing.T) {
	s := baseSettings(1)
	s.Pools[0].Ephemeral = true
	s.Pools[0].DockerSocket = false
	s.Pools[0].WorkBase = "/srv/gha"
	s.Pools[0].ExtraEnv = "ADDITIONAL_PACKAGES=kubectl\nFOO=bar"
	s.Pools[0].Group = "prod"
	r, d, _ := newTestReconciler(t, s)
	reconcile(t, r)

	if len(d.specs) != 1 {
		t.Fatalf("expected 1 container, got %d", len(d.specs))
	}
	var spec ContainerSpec
	for _, sp := range d.specs {
		spec = sp
	}
	if spec.RestartPolicy != "no" {
		t.Errorf("ephemeral runners must not restart: %q", spec.RestartPolicy)
	}
	for key, want := range map[string]string{
		"RUNNER_EPHEMERAL": "true", "RUNNER_DISABLE_UPDATE": "true", "RUNNER_GROUP": "prod",
		"ADDITIONAL_PACKAGES": "kubectl", "FOO": "bar", "RUNNER_WORKDIR": "/srv/gha/" + spec.Name,
	} {
		if got, _ := envValue(spec.Env, key); got != want {
			t.Errorf("env %s: got %q, want %q", key, got, want)
		}
	}
	if len(spec.Binds) != 1 || spec.Binds[0] != "/srv/gha/"+spec.Name+":/srv/gha/"+spec.Name {
		t.Errorf("binds: %v (socket disabled, work base enabled)", spec.Binds)
	}
}

func TestReconcile_PullsImageWhenMissingLocally(t *testing.T) {
	r, d, _ := newTestReconciler(t, baseSettings(1))
	d.imageIDs = map[string]string{} // nothing pulled yet
	reconcile(t, r)

	if len(d.pulled) != 1 || d.pulled[0] != DefaultRunnerImage {
		t.Fatalf("expected one pull of %s, got %v", DefaultRunnerImage, d.pulled)
	}
	if d.pullAuth == nil || d.pullAuth.Username != "u" {
		t.Errorf("registry auth must be passed to the pull: %+v", d.pullAuth)
	}
	if len(d.specs) != 1 {
		t.Errorf("container should be created after the pull, got %d", len(d.specs))
	}
	reconcile(t, r)
	if len(d.pulled) != 1 {
		t.Errorf("image must not be pulled again once present: %v", d.pulled)
	}
}

func TestReconcile_ScaleDownDrainsIdleOldestFirst(t *testing.T) {
	s := baseSettings(3)
	r, d, g := newTestReconciler(t, s)
	gen := gen(s)
	now := time.Now()
	busyID := d.add("corral-busy0000", "running", "healthy", gen, "sha256:current", now.Add(-3*time.Hour))
	idleNewID := d.add("corral-idle0new", "running", "healthy", gen, "sha256:current", now.Add(-1*time.Hour))
	idleOldID := d.add("corral-idle0old", "running", "healthy", gen, "sha256:current", now.Add(-2*time.Hour))
	g.runners = []Runner{
		{ID: 1, Name: "corral-busy0000", Status: "online", Busy: true},
		{ID: 2, Name: "corral-idle0new", Status: "online", Busy: false},
		{ID: 3, Name: "corral-idle0old", Status: "online", Busy: false},
	}

	if _, err := r.settings.Update(func(s *Settings) error { s.Pools[0].Count = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	r.WaitForDrains()

	if len(d.stops) != 1 || d.stops[0].id != idleOldID {
		t.Fatalf("expected exactly the oldest idle runner (%s) to be stopped, got %+v", idleOldID, d.stops)
	}
	if d.stops[0].timeout != 360*time.Second {
		t.Errorf("stop should wait graceful+60s (the entrypoint needs time after the job), got %v", d.stops[0].timeout)
	}
	if len(d.removed) != 1 || d.removed[0] != idleOldID {
		t.Errorf("removed: %v", d.removed)
	}
	if ids := g.deletedIDs(); len(ids) != 1 || ids[0] != 3 {
		t.Errorf("GitHub registration of the drained runner must be deleted, got %v", ids)
	}
	if d.byName("corral-busy0000") == nil || d.byName("corral-idle0new") == nil {
		t.Errorf("other runners must be untouched: %v", d.names())
	}
	_ = busyID
	_ = idleNewID

	reconcile(t, r)
	if len(d.specs) != 0 {
		t.Errorf("no new containers expected after scaling down, got %d", len(d.specs))
	}
}

func TestReconcile_ReplacesExitedContainersAndCleansGitHub(t *testing.T) {
	s := baseSettings(2)
	r, d, g := newTestReconciler(t, s)
	gen := gen(s)
	d.add("corral-alive000", "running", "healthy", gen, "sha256:current", time.Now())
	deadID := d.add("corral-dead0000", "exited", "", gen, "sha256:current", time.Now())
	g.runners = []Runner{{ID: 7, Name: "corral-dead0000", Status: "offline"}, {ID: 8, Name: "corral-alive000", Status: "online"}}

	reconcile(t, r)
	r.WaitForDrains()

	if len(d.removed) != 1 || d.removed[0] != deadID {
		t.Errorf("exited container must be removed, got %v", d.removed)
	}
	if ids := g.deletedIDs(); len(ids) != 1 || ids[0] != 7 {
		t.Errorf("its GitHub registration must be deleted, got %v", ids)
	}
	if len(d.specs) != 1 {
		t.Errorf("one replacement expected, got %d", len(d.specs))
	}
	if d.alive() != 2 {
		t.Errorf("should be back at 2 live runners, have %d", d.alive())
	}
}

func TestReconcile_RecreatesUnhealthyPersistentRunner(t *testing.T) {
	s := baseSettings(1)
	r, d, g := newTestReconciler(t, s)
	sickID := d.add("corral-sick0000", "running", "unhealthy", gen(s), "sha256:current", time.Now())
	g.runners = []Runner{{ID: 5, Name: "corral-sick0000", Status: "offline"}}

	reconcile(t, r)
	r.WaitForDrains()
	reconcile(t, r)

	if len(d.removed) != 1 || d.removed[0] != sickID {
		t.Errorf("unhealthy container must be removed, got %v", d.removed)
	}
	if len(d.specs) != 1 || d.alive() != 1 {
		t.Errorf("expected exactly one fresh replacement, specs=%d alive=%d", len(d.specs), d.alive())
	}
	if ids := g.deletedIDs(); len(ids) != 1 || ids[0] != 5 {
		t.Errorf("stale registration must be deleted, got %v", ids)
	}
}

func TestReconcile_RollingReplacementOnGenerationChange(t *testing.T) {
	s := baseSettings(2)
	r, d, g := newTestReconciler(t, s)
	d.add("corral-old00001", "running", "healthy", "oldgen", "sha256:current", time.Now().Add(-2*time.Hour))
	d.add("corral-old00002", "running", "healthy", "oldgen", "sha256:current", time.Now().Add(-1*time.Hour))
	g.runners = []Runner{{ID: 1, Name: "corral-old00001", Status: "online"}, {ID: 2, Name: "corral-old00002", Status: "online"}}

	// Each pass drains one outdated runner and immediately creates its replacement.
	for i := 0; i < 4; i++ {
		reconcile(t, r)
		r.WaitForDrains()
	}

	for _, c := range d.names() {
		if cont := d.byName(c); cont.Labels[labelGeneration] != gen(s) {
			t.Errorf("%s still on generation %q", c, cont.Labels[labelGeneration])
		}
	}
	if d.alive() != 2 {
		t.Errorf("expected 2 live runners at the end, got %d", d.alive())
	}
	if d.maxAlive > 3 {
		t.Errorf("rolling replacement must never exceed count+1 containers, peaked at %d", d.maxAlive)
	}
	if len(d.removed) != 2 {
		t.Errorf("both outdated runners should be gone, removed=%v", d.removed)
	}
}

func TestReconcile_RollsRunnersBuiltFromAnOlderImage(t *testing.T) {
	s := baseSettings(1)
	r, d, g := newTestReconciler(t, s)
	d.add("corral-oldimage", "running", "healthy", gen(s), "sha256:previous", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-oldimage", Status: "online"}}

	reconcile(t, r)
	r.WaitForDrains()
	reconcile(t, r)

	if d.byName("corral-oldimage") != nil {
		t.Error("runner built from the previous image should have been replaced")
	}
	if d.alive() != 1 {
		t.Errorf("expected 1 live runner, got %d", d.alive())
	}
}

func TestReconcile_WaitsForBusyRunnerBeforeRollingIt(t *testing.T) {
	s := baseSettings(1)
	r, d, g := newTestReconciler(t, s)
	d.add("corral-busyold0", "running", "healthy", "oldgen", "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-busyold0", Status: "online", Busy: true}}

	reconcile(t, r)
	r.WaitForDrains()

	if len(d.stops) != 0 {
		t.Errorf("a busy runner must not be rolled, got stops %+v", d.stops)
	}
	if len(d.specs) != 0 {
		t.Errorf("no replacement while the outdated runner still serves its job, got %d", len(d.specs))
	}
}

func TestReconcile_GarbageCollectsOfflineRegistrationsWithoutContainers(t *testing.T) {
	s := baseSettings(1)
	r, d, g := newTestReconciler(t, s)
	d.add("corral-alive000", "running", "healthy", gen(s), "sha256:current", time.Now())
	g.runners = []Runner{
		{ID: 1, Name: "corral-alive000", Status: "online"},
		{ID: 2, Name: "corral-ghost000", Status: "offline"}, // ours, gone
		{ID: 3, Name: "corral-foreign0", Status: "online"},  // ours by prefix but alive elsewhere: keep
		{ID: 4, Name: "laptop-runner", Status: "offline"},   // not ours: keep
	}

	reconcile(t, r)
	r.WaitForDrains()

	if ids := g.deletedIDs(); len(ids) != 1 || ids[0] != 2 {
		t.Errorf("only the offline ghost should be deleted, got %v", ids)
	}
}

func TestDestroy_DecrementsCountAndDrainsThatRunner(t *testing.T) {
	s := baseSettings(2)
	r, d, g := newTestReconciler(t, s)
	keepID := d.add("corral-keep0000", "running", "healthy", gen(s), "sha256:current", time.Now())
	goneID := d.add("corral-gone0000", "running", "healthy", gen(s), "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-keep0000", Status: "online"}, {ID: 2, Name: "corral-gone0000", Status: "online", Busy: true}}

	if err := r.Destroy(context.Background(), "corral-gone0000"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r.WaitForDrains()
	reconcile(t, r)

	if count(r, "ubuntu") != 1 {
		t.Errorf("count should drop to 1, got %d", count(r, "ubuntu"))
	}
	if len(d.removed) != 1 || d.removed[0] != goneID {
		t.Errorf("removed: %v", d.removed)
	}
	if ids := g.deletedIDs(); len(ids) != 1 || ids[0] != 2 {
		t.Errorf("GitHub deletion: %v", ids)
	}
	if d.byName("corral-keep0000") == nil || len(d.specs) != 0 {
		t.Errorf("the other runner must stay and nothing new be created; names=%v specs=%d", d.names(), len(d.specs))
	}
	_ = keepID

	if err := r.Destroy(context.Background(), "corral-nope0000"); err == nil {
		t.Error("destroying an unknown runner must fail")
	}
}

func TestRecreate_ReplacesRunnerKeepingCount(t *testing.T) {
	s := baseSettings(1)
	r, d, g := newTestReconciler(t, s)
	oldID := d.add("corral-old00000", "running", "healthy", gen(s), "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-old00000", Status: "online"}}

	if err := r.Recreate(context.Background(), "corral-old00000"); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	r.WaitForDrains()
	reconcile(t, r)

	if count(r, "ubuntu") != 1 {
		t.Errorf("count must be unchanged, got %d", count(r, "ubuntu"))
	}
	if len(d.removed) != 1 || d.removed[0] != oldID {
		t.Errorf("old container should be removed: %v", d.removed)
	}
	if len(d.specs) != 1 || d.alive() != 1 {
		t.Errorf("one fresh replacement expected, specs=%d alive=%d", len(d.specs), d.alive())
	}
}

func TestReconcile_DrainingRunnerIsReplacedImmediatelyButNotDrainedTwice(t *testing.T) {
	s := baseSettings(1)
	r, d, g := newTestReconciler(t, s)
	d.add("corral-slow0000", "running", "healthy", "oldgen", "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-slow0000", Status: "online"}}
	d.stopBlock = make(chan struct{}) // the runner is finishing a long job

	reconcile(t, r) // starts draining the outdated runner in the background
	deadline := time.Now().Add(5 * time.Second)
	for d.stopCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	reconcile(t, r) // while the drain is in progress

	if n := d.stopCount(); n != 1 {
		t.Errorf("draining runner must not be stopped twice, got %d stops", n)
	}
	if len(d.specs) != 1 {
		t.Errorf("replacement should be created while the old runner drains, got %d", len(d.specs))
	}
	close(d.stopBlock)
	r.WaitForDrains()
	reconcile(t, r)
	if d.alive() != 1 || len(d.specs) != 1 {
		t.Errorf("after the drain there must be exactly one runner, alive=%d specs=%d", d.alive(), len(d.specs))
	}
}

func TestReconcile_TokenFailureIsReportedNotFatal(t *testing.T) {
	r, d, g := newTestReconciler(t, baseSettings(2))
	g.tokenErr = errors.New("github api returned 401 for /orgs/example-org/actions/runners/registration-token: Bad credentials")

	err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("the pass should report the failure")
	}
	if len(d.specs) != 0 {
		t.Errorf("no container may be created without a token, got %d", len(d.specs))
	}
	found := false
	for _, e := range r.events.List() {
		if strings.Contains(e.Message, "Bad credentials") {
			found = true
		}
	}
	if !found {
		t.Errorf("failure should appear in the event log: %+v", r.events.List())
	}
}

func TestReconcile_GitHubOutageDoesNotBlockScaling(t *testing.T) {
	r, d, g := newTestReconciler(t, baseSettings(1))
	g.listErr = errors.New("github api: 503")

	reconcile(t, r)

	if len(d.specs) != 1 {
		t.Errorf("runners should still be created when the runner list is unavailable, got %d", len(d.specs))
	}
}

func TestSnapshot_MergesDockerAndGitHubState(t *testing.T) {
	s := baseSettings(2)
	r, d, g := newTestReconciler(t, s)
	d.add("corral-one00000", "running", "healthy", gen(s), "sha256:current", time.Now().Add(-time.Hour))
	d.add("corral-two00000", "running", "healthy", "oldgen", "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-one00000", Status: "online", Busy: true}}

	snap, err := r.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Pools) != 1 || snap.Pools[0].Count != 2 || snap.Target != "example-org" || len(snap.Pools[0].Runners) != 2 || len(snap.Orphans) != 0 {
		t.Fatalf("snapshot: %+v", snap)
	}
	if snap.Pools[0].Running != 2 || snap.Pools[0].Busy != 1 || snap.Pools[0].EffectiveImage != DefaultRunnerImage {
		t.Errorf("pool view: %+v", snap.Pools[0])
	}
	byName := map[string]RunnerView{}
	for _, v := range snap.Pools[0].Runners {
		byName[v.Name] = v
	}
	one := byName["corral-one00000"]
	if one.GitHubStatus != "online" || !one.Busy || one.Stale || one.Pool != "ubuntu" {
		t.Errorf("one: %+v", one)
	}
	two := byName["corral-two00000"]
	if two.GitHubStatus != "unregistered" || !two.Stale {
		t.Errorf("two: %+v", two)
	}
}

func twoPoolSettings() Settings {
	s := baseSettings(1)
	s.SetPool(Pool{Name: "debian", Runtime: "debian", Count: 2, Labels: "deb", DockerSocket: true, GracefulStopSeconds: 120})
	return s
}

func mustPool(s Settings, name string) Pool {
	p, ok := s.Pool(name)
	if !ok {
		panic("no pool " + name)
	}
	return p
}

func TestReconcile_EachPoolGetsItsOwnRunnersImageAndLabels(t *testing.T) {
	s := twoPoolSettings()
	r, d, _ := newTestReconciler(t, s)
	d.imageIDs[RunnerImageRepo+":debian"] = "sha256:debian"
	reconcile(t, r)

	if len(d.specs) != 3 {
		t.Fatalf("expected 3 containers (1 ubuntu + 2 debian), got %d", len(d.specs))
	}
	perPool := map[string]int{}
	for _, spec := range d.specs {
		pool := spec.Labels[labelPool]
		perPool[pool]++
		wantImage, wantLabels, wantStop := RunnerImageRepo+":ubuntu", "docker", 360
		if pool == "debian" {
			wantImage, wantLabels, wantStop = RunnerImageRepo+":debian", "deb", 180
		}
		if spec.Image != wantImage || spec.StopTimeout != wantStop || !strings.HasPrefix(spec.Name, "corral-"+pool+"-") {
			t.Errorf("%s: image=%s stop=%d name=%s", pool, spec.Image, spec.StopTimeout, spec.Name)
		}
		if got, _ := envValue(spec.Env, "RUNNER_LABELS"); got != wantLabels {
			t.Errorf("%s: RUNNER_LABELS=%q", pool, got)
		}
		if spec.Labels[labelGeneration] != mustPool(s, pool).Generation() {
			t.Errorf("%s: generation label %q", pool, spec.Labels[labelGeneration])
		}
	}
	if perPool["ubuntu"] != 1 || perPool["debian"] != 2 {
		t.Errorf("per pool: %v", perPool)
	}
	reconcile(t, r)
	if len(d.specs) != 3 {
		t.Errorf("second pass must be a no-op, got %d specs", len(d.specs))
	}
}

func TestReconcile_AdoptsUnlabeledContainersIntoFirstPool(t *testing.T) {
	s := twoPoolSettings()
	r, d, g := newTestReconciler(t, s)
	d.imageIDs[RunnerImageRepo+":debian"] = "sha256:debian"
	d.add("corral-94265b50", "running", "healthy", gen(s), "sha256:current", time.Now()) // pre-pool container
	g.runners = []Runner{{ID: 1, Name: "corral-94265b50", Status: "online"}}

	reconcile(t, r)
	r.WaitForDrains()

	if d.byName("corral-94265b50") == nil {
		t.Fatal("an unlabeled container with the current generation must be kept as a member of the first pool")
	}
	if len(d.specs) != 2 {
		t.Errorf("only the debian pool should create runners, got %d specs", len(d.specs))
	}
	snap, _ := r.Snapshot(context.Background())
	if len(snap.Pools[0].Runners) != 1 || snap.Pools[0].Runners[0].Pool != "ubuntu" || len(snap.Orphans) != 0 {
		t.Errorf("adopted runner must show under the first pool: %+v", snap)
	}
}

func TestReconcile_OrphansOfRemovedPoolsDrainOneAtATime(t *testing.T) {
	s := baseSettings(1)
	r, d, g := newTestReconciler(t, s)
	keep := d.add("corral-ubuntu-aaaaaa", "running", "healthy", gen(s), "sha256:current", time.Now())
	d.addInPool("gone", "corral-gone-000001", "running", "healthy", "x", "sha256:current", time.Now().Add(-2*time.Hour))
	d.addInPool("gone", "corral-gone-000002", "running", "healthy", "x", "sha256:current", time.Now().Add(-time.Hour))
	g.runners = []Runner{{ID: 1, Name: "corral-gone-000001", Status: "online"}, {ID: 2, Name: "corral-gone-000002", Status: "online", Busy: true}}

	reconcile(t, r)
	r.WaitForDrains()
	if len(d.removed) != 1 || d.byName("corral-gone-000001") != nil {
		t.Fatalf("the idle orphan must be drained first, removed=%v", d.removed)
	}
	if d.byName("corral-gone-000002") == nil {
		t.Fatal("the busy orphan must wait")
	}
	if len(d.specs) != 0 {
		t.Errorf("orphans are not replaced, got %d specs", len(d.specs))
	}
	found := false
	for _, e := range r.events.List() {
		if strings.Contains(e.Message, "pool removed") {
			found = true
		}
	}
	if !found {
		t.Error("drain reason should be 'pool removed'")
	}
	// The drain deleted the first registration; the busy one goes idle now.
	g.runners = []Runner{{ID: 2, Name: "corral-gone-000002", Status: "online", Busy: false}}
	reconcile(t, r)
	r.WaitForDrains()
	if d.byName("corral-gone-000002") != nil || d.byName("corral-ubuntu-aaaaaa") == nil {
		t.Errorf("second orphan drained once idle, member untouched: %v", d.names())
	}
	_ = keep
}

func TestReconcile_RollsOneOutdatedRunnerPerPoolPerPass(t *testing.T) {
	s := twoPoolSettings()
	r, d, g := newTestReconciler(t, s)
	d.imageIDs[RunnerImageRepo+":debian"] = "sha256:debian"
	d.addInPool("ubuntu", "corral-ubuntu-old001", "running", "healthy", "oldgen", "sha256:current", time.Now())
	d.addInPool("debian", "corral-debian-old001", "running", "healthy", "oldgen", "sha256:debian", time.Now().Add(-time.Hour))
	d.addInPool("debian", "corral-debian-old002", "running", "healthy", "oldgen", "sha256:debian", time.Now())
	g.runners = []Runner{
		{ID: 1, Name: "corral-ubuntu-old001", Status: "online"},
		{ID: 2, Name: "corral-debian-old001", Status: "online"},
		{ID: 3, Name: "corral-debian-old002", Status: "online"},
	}
	d.stopBlock = make(chan struct{})

	reconcile(t, r)
	deadline := time.Now().Add(5 * time.Second)
	for d.stopCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := d.stopCount(); n != 2 {
		t.Fatalf("one outdated runner per pool should drain in the first pass, got %d", n)
	}
	reconcile(t, r) // pools with a drain in progress must not start another
	if n := d.stopCount(); n != 2 {
		t.Errorf("no second drain while one is in progress in that pool, got %d", n)
	}
	close(d.stopBlock)
	r.WaitForDrains()
	for i := 0; i < 4; i++ {
		reconcile(t, r)
		r.WaitForDrains()
	}
	for _, name := range d.names() {
		c := d.byName(name)
		if c.Labels[labelGeneration] != mustPool(s, c.Labels[labelPool]).Generation() {
			t.Errorf("%s still on generation %q", name, c.Labels[labelGeneration])
		}
	}
	if d.alive() != 3 {
		t.Errorf("expected 3 live runners at the end, got %d", d.alive())
	}
}

func TestDestroy_LowersTheRightPoolCount(t *testing.T) {
	s := twoPoolSettings()
	r, d, g := newTestReconciler(t, s)
	d.imageIDs[RunnerImageRepo+":debian"] = "sha256:debian"
	d.addInPool("debian", "corral-debian-000001", "running", "healthy", mustPool(s, "debian").Generation(), "sha256:debian", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-debian-000001", Status: "online"}}

	if err := r.Destroy(context.Background(), "corral-debian-000001"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r.WaitForDrains()
	if count(r, "debian") != 1 || count(r, "ubuntu") != 1 {
		t.Errorf("debian count should drop to 1 and ubuntu stay 1: debian=%d ubuntu=%d", count(r, "debian"), count(r, "ubuntu"))
	}
	if len(d.stops) != 1 || d.stops[0].timeout != 180*time.Second {
		t.Errorf("drain must use the pool's graceful stop (120+60s): %+v", d.stops)
	}
}

func TestPullAndRoll_PullsDistinctImagesOnceOrOnePool(t *testing.T) {
	s := twoPoolSettings()
	s.SetPool(Pool{Name: "ubuntu2", Runtime: "ubuntu", Count: 0, DockerSocket: true})
	r, d, _ := newTestReconciler(t, s)

	if err := r.PullAndRoll(context.Background(), ""); err != nil {
		t.Fatalf("PullAndRoll all: %v", err)
	}
	if len(d.pulled) != 2 || d.pulled[0] != RunnerImageRepo+":debian" || d.pulled[1] != RunnerImageRepo+":ubuntu" {
		t.Errorf("all pools: expected the two distinct images once each, got %v", d.pulled)
	}
	d.pulled = nil
	if err := r.PullAndRoll(context.Background(), "debian"); err != nil || len(d.pulled) != 1 || d.pulled[0] != RunnerImageRepo+":debian" {
		t.Errorf("one pool: %v %v", d.pulled, err)
	}
	if err := r.PullAndRoll(context.Background(), "nope"); err == nil {
		t.Error("unknown pool must be an error")
	}
	select {
	case <-r.wake:
	default:
		t.Error("a pull must wake the reconciler")
	}
}

func TestEnsureImage_NeverClearsAPullFlagItDidNotSet(t *testing.T) {
	s := twoPoolSettings()
	r, d, _ := newTestReconciler(t, s)
	// debian image absent locally, a manual pull / update check in flight
	r.pulling.Store(true)
	reconcile(t, r)
	if !r.pulling.Load() {
		t.Error("a pass that pulls a missing image must not clear the flag held by another pull")
	}
	if len(d.pulled) != 1 || d.pulled[0] != RunnerImageRepo+":debian" {
		t.Errorf("the missing image must still be pulled so the pool can start: %v", d.pulled)
	}
	r.pulling.Store(false)
	d.imageIDs[RunnerImageRepo+":minideb"] = ""
	if _, err := r.settings.Update(func(s *Settings) error {
		s.SetPool(Pool{Name: "m", Runtime: "minideb", DockerSocket: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if r.pulling.Load() {
		t.Error("a pass that set the flag itself must clear it again")
	}
}

func TestStartPullAndRoll_RefusesWhileAnotherPullRuns(t *testing.T) {
	r, d, _ := newTestReconciler(t, twoPoolSettings())
	r.pulling.Store(true)
	if err := r.StartPullAndRoll(""); !errors.Is(err, errPullInProgress) {
		t.Errorf("expected errPullInProgress, got %v", err)
	}
	if err := r.PullAndRoll(context.Background(), "debian"); !errors.Is(err, errPullInProgress) {
		t.Errorf("expected errPullInProgress, got %v", err)
	}
	r.pulling.Store(false)
	if err := r.StartPullAndRoll("nope"); err == nil || errors.Is(err, errPullInProgress) {
		t.Errorf("unknown pool must be reported as such, got %v", err)
	}
	if err := r.StartPullAndRoll("debian"); err != nil {
		t.Fatalf("StartPullAndRoll: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		n := len(d.pulled)
		d.mu.Unlock()
		if n == 1 && !r.pulling.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("background pull should run once and release the flag: pulled=%v pulling=%v", d.pulled, r.pulling.Load())
}

func TestOwnership_IsByLabelsNotName(t *testing.T) {
	r, _, _ := newTestReconciler(t, baseSettings(0))
	mine := Container{Name: "whatever-1", Labels: map[string]string{labelManaged: "true", labelTarget: "example-org"}}
	other := Container{Name: "corral-ubuntu-aaaaaa", Labels: map[string]string{labelManaged: "true", labelTarget: "other-org"}}
	unlabeled := Container{Name: "corral-ubuntu-bbbbbb", Labels: map[string]string{}}
	if !r.ownsContainer(mine) || r.ownsContainer(other) || r.ownsContainer(unlabeled) {
		t.Errorf("ownership: mine=%v other=%v unlabeled=%v", r.ownsContainer(mine), r.ownsContainer(other), r.ownsContainer(unlabeled))
	}
}
