package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	labelManaged    = "dev.oease.gha.managed"
	labelName       = "dev.oease.gha.name"
	labelGeneration = "dev.oease.gha.generation"
	labelEphemeral  = "dev.oease.gha.ephemeral"
	labelTarget     = "dev.oease.gha.target"
	managedFilter   = labelManaged + "=true"
)

type dockerAPI interface {
	ListContainers(ctx context.Context, labelFilter string) ([]Container, error)
	InspectContainer(ctx context.Context, id string) (Container, error)
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	PullImage(ctx context.Context, ref string, auth *RegistryAuth) error
	ImageID(ctx context.Context, ref string) (string, error)
}

type githubAPI interface {
	RegistrationToken(ctx context.Context) (string, error)
	ListRunners(ctx context.Context) ([]Runner, error)
	DeleteRunner(ctx context.Context, id int64) error
}

// ControllerConfig is the static (environment-only) part of the configuration.
type ControllerConfig struct {
	Target       Target
	NamePrefix   string
	GitHubURL    string
	RegistryAuth *RegistryAuth
	DockerSocket string // host socket path mounted into runners
	Interval     time.Duration
}

type drainInfo struct {
	Reason  string
	Started time.Time
}

// Reconciler keeps the set of runner containers equal to the desired state.
type Reconciler struct {
	docker   dockerAPI
	github   githubAPI
	settings *SettingsStore
	cfg      ControllerConfig
	events   *EventLog

	mu sync.Mutex // serializes reconcile passes and operator actions

	dmu      sync.Mutex // guards draining
	draining map[string]drainInfo
	drainWG  sync.WaitGroup

	wake         chan struct{}
	pulling      atomic.Bool
	githubErr    atomic.Value // string
	lastPassTime atomic.Value // time.Time
}

func NewReconciler(docker dockerAPI, github githubAPI, settings *SettingsStore, cfg ControllerConfig, events *EventLog) *Reconciler {
	if cfg.NamePrefix == "" {
		cfg.NamePrefix = "oease"
	}
	if cfg.DockerSocket == "" {
		cfg.DockerSocket = "/var/run/docker.sock"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	r := &Reconciler{
		docker: docker, github: github, settings: settings, cfg: cfg, events: events,
		draining: map[string]drainInfo{},
		wake:     make(chan struct{}, 1),
	}
	r.githubErr.Store("")
	r.lastPassTime.Store(time.Time{})
	return r
}

// Run reconciles on a timer and whenever Wake is called, until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		_ = r.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
	}
}

// Wake requests an immediate reconcile pass.
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// WaitForDrains blocks until all in-flight drains finished (used by tests and shutdown).
func (r *Reconciler) WaitForDrains() { r.drainWG.Wait() }

func (r *Reconciler) isDraining(name string) (drainInfo, bool) {
	r.dmu.Lock()
	defer r.dmu.Unlock()
	d, ok := r.draining[name]
	return d, ok
}

func (r *Reconciler) drainingNames() map[string]bool {
	r.dmu.Lock()
	defer r.dmu.Unlock()
	out := make(map[string]bool, len(r.draining))
	for n := range r.draining {
		out[n] = true
	}
	return out
}

func runnerName(c Container) string {
	if n := c.Labels[labelName]; n != "" {
		return n
	}
	return c.Name
}

func isEphemeral(c Container) bool { return c.Labels[labelEphemeral] == "true" }

func (r *Reconciler) ownsContainer(c Container) bool {
	if t, ok := c.Labels[labelTarget]; ok && t != r.cfg.Target.String() {
		return false
	}
	return strings.HasPrefix(runnerName(c), r.cfg.NamePrefix+"-")
}

// Reconcile performs one pass: observe, clean up, scale, roll, garbage-collect.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.lastPassTime.Store(time.Now())

	s := r.settings.Get()
	gen := s.Generation()

	all, err := r.docker.ListContainers(ctx, managedFilter)
	if err != nil {
		r.events.Errorf("docker: list containers: %v", err)
		return err
	}
	var containers []Container
	for _, c := range all {
		if r.ownsContainer(c) {
			containers = append(containers, c)
		}
	}

	ghRunners, ghErr := r.github.ListRunners(ctx)
	ghByName := map[string]Runner{}
	if ghErr != nil {
		if r.githubErr.Load().(string) == "" {
			r.events.Warnf("github: cannot list runners (continuing without runner status): %v", ghErr)
		}
		r.githubErr.Store(ghErr.Error())
	} else {
		r.githubErr.Store("")
		for _, g := range ghRunners {
			ghByName[g.Name] = g
		}
	}

	imageID, err := r.ensureImage(ctx, s.Image)
	if err != nil {
		r.events.Errorf("image %s: %v", s.Image, err)
		return err
	}

	var errs []error
	var members []Container
	existing := map[string]bool{}
	draining := r.drainingNames()
	for _, c := range containers {
		name := runnerName(c)
		existing[name] = true
		if draining[name] {
			continue
		}
		switch c.State {
		case "exited", "dead":
			if isEphemeral(c) {
				r.events.Infof("%s: finished (%s); replacing", name, c.Status)
			} else {
				r.events.Warnf("%s: %s; recreating", name, c.Status)
			}
			r.removeContainerAndRegistration(ctx, c, ghByName)
		case "created":
			if err := r.docker.StartContainer(ctx, c.ID); err != nil {
				r.events.Errorf("%s: cannot start: %v; removing", name, err)
				r.removeContainerAndRegistration(ctx, c, ghByName)
				continue
			}
			members = append(members, c)
		default: // running, restarting, paused
			if c.Health == "unhealthy" && !isEphemeral(c) {
				r.startDrain(name, c.ID, "unhealthy", s)
				continue
			}
			members = append(members, c)
		}
	}

	// GitHub-side garbage collection: our registrations that lost their container.
	if ghErr == nil {
		for _, g := range ghRunners {
			if !strings.HasPrefix(g.Name, r.cfg.NamePrefix+"-") || g.Status != "offline" || existing[g.Name] || draining[g.Name] {
				continue
			}
			if err := r.github.DeleteRunner(ctx, g.ID); err != nil {
				r.events.Errorf("%s: delete stale registration: %v", g.Name, err)
				continue
			}
			r.events.Infof("%s: removed stale GitHub registration", g.Name)
		}
	}

	switch {
	case len(members) < s.Count:
		for i := len(members); i < s.Count; i++ {
			if err := r.createRunner(ctx, s, gen, existing); err != nil {
				r.events.Errorf("create runner: %v", err)
				errs = append(errs, err)
				break
			}
		}
	case len(members) > s.Count:
		candidates := sortForRemoval(members, ghByName)
		for _, c := range candidates[:len(members)-s.Count] {
			r.startDrain(runnerName(c), c.ID, "scaling down", s)
		}
	case len(draining) == 0:
		// Rolling replacement: one outdated, idle runner per pass; its
		// replacement is created on the next pass while it drains.
		for _, c := range sortForRemoval(members, ghByName) {
			if !isStale(c, gen, imageID) {
				continue
			}
			if g, ok := ghByName[runnerName(c)]; ok && g.Busy {
				continue
			}
			r.startDrain(runnerName(c), c.ID, "outdated", s)
			break
		}
	}
	return errors.Join(errs...)
}

func isStale(c Container, gen, imageID string) bool {
	if c.Labels[labelGeneration] != gen {
		return true
	}
	return imageID != "" && c.ImageID != "" && c.ImageID != imageID
}

// sortForRemoval orders runners so the cheapest to remove come first:
// idle (or unknown) before busy, then oldest first.
func sortForRemoval(members []Container, gh map[string]Runner) []Container {
	out := append([]Container(nil), members...)
	busy := func(c Container) bool {
		g, ok := gh[runnerName(c)]
		return ok && g.Busy
	}
	sort.SliceStable(out, func(i, j int) bool {
		bi, bj := busy(out[i]), busy(out[j])
		if bi != bj {
			return !bi
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out
}

// ensureImage returns the local image id, pulling the image when absent.
func (r *Reconciler) ensureImage(ctx context.Context, image string) (string, error) {
	id, err := r.docker.ImageID(ctx, image)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if err := r.pull(ctx, image); err != nil {
		return "", err
	}
	return r.docker.ImageID(ctx, image)
}

func (r *Reconciler) pull(ctx context.Context, image string) error {
	r.pulling.Store(true)
	defer r.pulling.Store(false)
	r.events.Infof("pulling %s", image)
	pullCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := r.docker.PullImage(pullCtx, image, r.cfg.RegistryAuth); err != nil {
		return err
	}
	r.events.Infof("pulled %s", image)
	return nil
}

func (r *Reconciler) newName(existing map[string]bool) string {
	for {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		name := r.cfg.NamePrefix + "-" + hex.EncodeToString(b)
		if !existing[name] {
			return name
		}
	}
}

func (r *Reconciler) buildSpec(s Settings, gen, name, token string) ContainerSpec {
	env := []string{
		"RUNNER_TOKEN=" + token,
		"RUNNER_REGISTER_TO=" + r.cfg.Target.String(),
		"RUNNER_NAME=" + name,
		"RUNNER_LABELS=" + s.Labels,
		"RUNNER_GROUP=" + s.Group,
		"RUNNER_EPHEMERAL=" + strconv.FormatBool(s.Ephemeral),
		"RUNNER_DISABLE_UPDATE=" + strconv.FormatBool(s.Ephemeral),
		"RUNNER_GRACEFUL_STOP_TIMEOUT=" + strconv.Itoa(s.GracefulStopSeconds),
	}
	if r.cfg.GitHubURL != "" && strings.TrimRight(r.cfg.GitHubURL, "/") != "https://github.com" {
		env = append(env, "GITHUB_URL="+r.cfg.GitHubURL)
	}
	var binds []string
	if s.DockerSocket {
		binds = append(binds, r.cfg.DockerSocket+":/var/run/docker.sock")
	}
	if s.WorkBase != "" {
		dir := path.Join(s.WorkBase, name)
		binds = append(binds, dir+":"+dir)
		env = append(env, "RUNNER_WORKDIR="+dir)
	}
	env = append(env, s.ExtraEnvList()...)

	restart := "unless-stopped"
	if s.Ephemeral {
		restart = "no"
	}
	return ContainerSpec{
		Name:     name,
		Image:    s.Image,
		Hostname: name,
		Env:      env,
		Labels: map[string]string{
			labelManaged:    "true",
			labelName:       name,
			labelGeneration: gen,
			labelEphemeral:  strconv.FormatBool(s.Ephemeral),
			labelTarget:     r.cfg.Target.String(),
		},
		Binds:         binds,
		RestartPolicy: restart,
		StopTimeout:   s.GracefulStopSeconds + 60,
	}
}

func (r *Reconciler) createRunner(ctx context.Context, s Settings, gen string, existing map[string]bool) error {
	name := r.newName(existing)
	token, err := r.github.RegistrationToken(ctx)
	if err != nil {
		return fmt.Errorf("registration token: %w", err)
	}
	spec := r.buildSpec(s, gen, name, token)
	id, err := r.docker.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if err := r.docker.StartContainer(ctx, id); err != nil {
		_ = r.docker.RemoveContainer(ctx, id, true)
		return fmt.Errorf("start %s: %w", name, err)
	}
	existing[name] = true
	r.events.Infof("%s: created (%s)", name, s.Image)
	return nil
}

func (r *Reconciler) removeContainerAndRegistration(ctx context.Context, c Container, gh map[string]Runner) {
	name := runnerName(c)
	if err := r.docker.RemoveContainer(ctx, c.ID, true); err != nil {
		r.events.Errorf("%s: remove container: %v", name, err)
		return
	}
	if g, ok := gh[name]; ok {
		if err := r.github.DeleteRunner(ctx, g.ID); err != nil {
			r.events.Errorf("%s: delete registration: %v", name, err)
		}
	}
}

// startDrain retires a runner in the background: stop (the entrypoint waits
// for a running job), remove the container, delete the GitHub registration.
func (r *Reconciler) startDrain(name, id, reason string, s Settings) {
	r.dmu.Lock()
	if _, already := r.draining[name]; already {
		r.dmu.Unlock()
		return
	}
	r.draining[name] = drainInfo{Reason: reason, Started: time.Now()}
	r.dmu.Unlock()

	r.events.Infof("%s: draining (%s)", name, reason)
	stopTimeout := time.Duration(s.GracefulStopSeconds+60) * time.Second
	r.drainWG.Add(1)
	go func() {
		defer r.drainWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout+5*time.Minute)
		defer cancel()
		if err := r.docker.StopContainer(ctx, id, stopTimeout); err != nil {
			r.events.Errorf("%s: stop: %v", name, err)
		}
		if err := r.docker.RemoveContainer(ctx, id, true); err != nil {
			r.events.Errorf("%s: remove: %v", name, err)
		}
		r.deleteRegistrationByName(ctx, name)
		r.dmu.Lock()
		delete(r.draining, name)
		r.dmu.Unlock()
		r.events.Infof("%s: removed", name)
		r.Wake()
	}()
}

func (r *Reconciler) deleteRegistrationByName(ctx context.Context, name string) {
	runners, err := r.github.ListRunners(ctx)
	if err != nil {
		r.events.Warnf("%s: cannot look up its registration (will be garbage-collected later): %v", name, err)
		return
	}
	for _, g := range runners {
		if g.Name == name {
			if err := r.github.DeleteRunner(ctx, g.ID); err != nil {
				r.events.Errorf("%s: delete registration: %v", name, err)
			}
			return
		}
	}
}

func (r *Reconciler) findContainer(ctx context.Context, name string) (Container, error) {
	containers, err := r.docker.ListContainers(ctx, managedFilter)
	if err != nil {
		return Container{}, err
	}
	for _, c := range containers {
		if runnerName(c) == name && r.ownsContainer(c) {
			return c, nil
		}
	}
	return Container{}, fmt.Errorf("no runner named %q", name)
}

// Destroy removes one runner for good and lowers the desired count.
func (r *Reconciler) Destroy(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.findContainer(ctx, name)
	if err != nil {
		return err
	}
	s, err := r.settings.Update(func(s *Settings) error {
		if s.Count > 0 {
			s.Count--
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.startDrain(name, c.ID, "destroyed by operator", s)
	return nil
}

// Recreate retires one runner; the next pass replaces it with a fresh one.
func (r *Reconciler) Recreate(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.findContainer(ctx, name)
	if err != nil {
		return err
	}
	r.startDrain(name, c.ID, "recreate requested", r.settings.Get())
	return nil
}

// PullAndRoll pulls the configured image; runners built from an older image
// are then replaced one at a time by the reconcile loop.
func (r *Reconciler) PullAndRoll(ctx context.Context) error {
	if r.pulling.Load() {
		return errors.New("a pull is already in progress")
	}
	image := r.settings.Get().Image
	if err := r.pull(ctx, image); err != nil {
		r.events.Errorf("pull %s: %v", image, err)
		return err
	}
	r.Wake()
	return nil
}

// RunnerView is one runner as shown in the UI.
type RunnerView struct {
	Name         string    `json:"name"`
	ContainerID  string    `json:"container_id"`
	State        string    `json:"state"`
	Health       string    `json:"health"`
	Status       string    `json:"status"`
	Created      time.Time `json:"created"`
	Image        string    `json:"image"`
	Generation   string    `json:"generation"`
	Stale        bool      `json:"stale"`
	Draining     bool      `json:"draining"`
	DrainReason  string    `json:"drain_reason,omitempty"`
	GitHubStatus string    `json:"github_status"`
	Busy         bool      `json:"busy"`
	Ephemeral    bool      `json:"ephemeral"`
}

// State is everything the UI needs.
type State struct {
	Version     string       `json:"version"`
	Target      string       `json:"target"`
	GitHubURL   string       `json:"github_url"`
	Settings    Settings     `json:"settings"`
	Generation  string       `json:"generation"`
	Runners     []RunnerView `json:"runners"`
	Events      []Event      `json:"events"`
	Pulling     bool         `json:"pulling"`
	GitHubError string       `json:"github_error,omitempty"`
	DockerError string       `json:"docker_error,omitempty"`
	LastPass    time.Time    `json:"last_pass"`
	Now         time.Time    `json:"now"`
}

// Snapshot merges Docker and GitHub views for the dashboard (read-only).
func (r *Reconciler) Snapshot(ctx context.Context) (State, error) {
	s := r.settings.Get()
	st := State{
		Version: version, Target: r.cfg.Target.String(), GitHubURL: r.cfg.Target.URL(r.cfg.GitHubURL),
		Settings: s, Generation: s.Generation(), Events: r.events.List(),
		Pulling: r.pulling.Load(), GitHubError: r.githubErr.Load().(string),
		LastPass: r.lastPassTime.Load().(time.Time), Now: time.Now(),
	}
	containers, err := r.docker.ListContainers(ctx, managedFilter)
	if err != nil {
		return st, err
	}
	ghByName := map[string]Runner{}
	ghRunners, ghErr := r.github.ListRunners(ctx)
	if ghErr == nil {
		for _, g := range ghRunners {
			ghByName[g.Name] = g
		}
	} else if st.GitHubError == "" {
		st.GitHubError = ghErr.Error()
	}
	imageID, _ := r.docker.ImageID(ctx, s.Image)

	for _, c := range containers {
		if !r.ownsContainer(c) {
			continue
		}
		name := runnerName(c)
		v := RunnerView{
			Name: name, ContainerID: c.ID, State: c.State, Health: c.Health, Status: c.Status, Created: c.Created,
			Image: c.Image, Generation: c.Labels[labelGeneration], Stale: isStale(c, st.Generation, imageID),
			Ephemeral: isEphemeral(c), GitHubStatus: "unknown",
		}
		if d, ok := r.isDraining(name); ok {
			v.Draining, v.DrainReason = true, d.Reason
		}
		if ghErr == nil {
			if g, ok := ghByName[name]; ok {
				v.GitHubStatus, v.Busy = g.Status, g.Busy
			} else {
				v.GitHubStatus = "unregistered"
			}
		}
		st.Runners = append(st.Runners, v)
	}
	sort.Slice(st.Runners, func(i, j int) bool { return st.Runners[i].Created.Before(st.Runners[j].Created) })
	return st, nil
}
