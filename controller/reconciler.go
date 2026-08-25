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
	labelPrefix     = "corral."
	labelManaged    = labelPrefix + "managed"
	labelName       = labelPrefix + "name"
	labelGeneration = labelPrefix + "generation"
	labelEphemeral  = labelPrefix + "ephemeral"
	labelTarget     = labelPrefix + "target"
	labelPool       = labelPrefix + "pool"
	// Set on the images by the Dockerfile; Docker copies them onto containers.
	labelFlavor        = labelPrefix + "flavor"
	labelRunnerVersion = labelPrefix + "runner-version"
	managedFilter      = labelManaged + "=true"
)

type dockerAPI interface {
	ListContainers(ctx context.Context, labelFilter string) ([]Container, error)
	InspectContainer(ctx context.Context, id string) (Container, error)
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	PullImage(ctx context.Context, ref string, auth *RegistryAuth) error
	InspectImage(ctx context.Context, ref string) (ImageInfo, error)
}

type githubAPI interface {
	RegistrationToken(ctx context.Context) (string, error)
	ListRunners(ctx context.Context) ([]Runner, error)
	DeleteRunner(ctx context.Context, id int64) error
	LatestRunnerVersion(ctx context.Context) (string, error)
}

// ControllerConfig is the static (environment-only) part of the configuration.
type ControllerConfig struct {
	Target         Target
	NamePrefix     string
	GitHubURL      string
	RegistryAuth   *RegistryAuth
	DockerSocket   string        // host socket path mounted into runners
	Interval       time.Duration // reconcile interval
	UpdateInterval time.Duration // how often pool images are pulled for updates; 0 disables
	Flavors        []Flavor      // runner image catalog (images/*/flavor.json)
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

	// automatic image checks (update.go)
	lastUpdateCheck atomic.Value // time.Time
	latestRunner    atomic.Value // string
	updateWG        sync.WaitGroup
	now             func() time.Time
}

func NewReconciler(docker dockerAPI, github githubAPI, settings *SettingsStore, cfg ControllerConfig, events *EventLog) *Reconciler {
	if cfg.NamePrefix == "" {
		cfg.NamePrefix = "corral"
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
		now:      time.Now,
	}
	r.githubErr.Store("")
	r.lastPassTime.Store(time.Time{})
	r.lastUpdateCheck.Store(time.Time{})
	r.latestRunner.Store("")
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

// WaitForDrains blocks until all in-flight drains finished (tests and shutdown).
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

// ownsContainer: managed by a controller for the same target. The name prefix
// only decorates names; ownership is the labels' job.
func (r *Reconciler) ownsContainer(c Container) bool {
	if c.Labels[labelManaged] != "true" {
		return false
	}
	if t, ok := c.Labels[labelTarget]; ok && t != r.cfg.Target.String() {
		return false
	}
	return true
}

// listManaged returns every container this controller manages.
func (r *Reconciler) listManaged(ctx context.Context) ([]Container, error) {
	return r.docker.ListContainers(ctx, managedFilter)
}

// poolOf returns the pool a container belongs to. Containers created before
// pools existed carry no pool label and belong to the first pool.
func poolOf(c Container, s Settings) string {
	if p := c.Labels[labelPool]; p != "" {
		return p
	}
	if len(s.Pools) > 0 {
		return s.Pools[0].Name
	}
	return ""
}

// Reconcile performs one pass: observe, then per pool clean up, scale and
// roll; drain orphans; garbage-collect GitHub registrations.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.lastPassTime.Store(time.Now())

	s := r.settings.Get()

	all, err := r.listManaged(ctx)
	if err != nil {
		r.events.Errorf("docker: list containers: %v", err)
		return err
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

	byPool := map[string][]Container{}
	var orphans []Container
	existing := map[string]bool{}
	for _, c := range all {
		if !r.ownsContainer(c) {
			continue
		}
		existing[runnerName(c)] = true
		pool := poolOf(c, s)
		if _, ok := s.Pool(pool); ok {
			byPool[pool] = append(byPool[pool], c)
		} else {
			orphans = append(orphans, c)
		}
	}
	draining := r.drainingNames()

	var errs []error
	for _, p := range s.Pools {
		img, err := r.ensureImage(ctx, p.EffectiveImage())
		if err != nil {
			r.events.Errorf("pool %s: image %s: %v", p.Name, p.EffectiveImage(), err)
			errs = append(errs, err)
			continue
		}
		errs = append(errs, r.reconcilePool(ctx, p, img.ID, byPool[p.Name], ghByName, existing, draining)...)
	}

	// Orphans belong to pools that no longer exist: retire one per pass, idle first.
	if len(draining) == 0 {
		for _, c := range sortForRemoval(orphans, ghByName) {
			if g, ok := ghByName[runnerName(c)]; ok && g.Busy {
				continue
			}
			r.startDrain(runnerName(c), c.ID, "pool removed", defaultGracefulStopSeconds)
			break
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

	r.maybeCheckForUpdates(s)
	return errors.Join(errs...)
}

// reconcilePool applies the desired state of one pool to its containers.
func (r *Reconciler) reconcilePool(ctx context.Context, p Pool, imageID string, containers []Container, ghByName map[string]Runner, existing map[string]bool, draining map[string]bool) []error {
	gen := p.Generation()
	var errs []error
	var members []Container
	poolDraining := false
	for _, c := range containers {
		name := runnerName(c)
		if draining[name] {
			poolDraining = true
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
				r.startDrain(name, c.ID, "unhealthy", p.GracefulStopSeconds)
				poolDraining = true
				continue
			}
			members = append(members, c)
		}
	}

	switch {
	case len(members) < p.Count:
		for i := len(members); i < p.Count; i++ {
			if err := r.createRunner(ctx, p, gen, existing); err != nil {
				r.events.Errorf("pool %s: create runner: %v", p.Name, err)
				errs = append(errs, err)
				break
			}
		}
	case len(members) > p.Count:
		candidates := sortForRemoval(members, ghByName)
		for _, c := range candidates[:len(members)-p.Count] {
			r.startDrain(runnerName(c), c.ID, "scaling down", p.GracefulStopSeconds)
		}
	case !poolDraining:
		// Rolling replacement: one outdated, idle runner per pool per pass; its
		// replacement is created on the next pass while it drains.
		for _, c := range sortForRemoval(members, ghByName) {
			if !isStale(c, gen, imageID) {
				continue
			}
			if g, ok := ghByName[runnerName(c)]; ok && g.Busy {
				continue
			}
			r.startDrain(runnerName(c), c.ID, "outdated", p.GracefulStopSeconds)
			break
		}
	}
	return errs
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

// ensureImage returns the local image, pulling it when absent.
func (r *Reconciler) ensureImage(ctx context.Context, image string) (ImageInfo, error) {
	info, err := r.docker.InspectImage(ctx, image)
	if err != nil {
		return ImageInfo{}, err
	}
	if info.ID != "" {
		return info, nil
	}
	// Show the pull in the UI, but never clear a flag another pull holds: a
	// missing image must not block the pass, and the daemon copes with two
	// pulls of the same image.
	if r.pulling.CompareAndSwap(false, true) {
		defer r.pulling.Store(false)
	}
	if err := r.pull(ctx, image); err != nil {
		return ImageInfo{}, err
	}
	return r.docker.InspectImage(ctx, image)
}

// pull downloads an image and logs it; callers manage the pulling flag.
func (r *Reconciler) pull(ctx context.Context, image string) error {
	r.events.Infof("pulling %s", image)
	if err := r.pullImage(ctx, image); err != nil {
		return err
	}
	r.events.Infof("pulled %s", image)
	return nil
}

// pullImage downloads an image without logging (hourly checks would flood the log).
func (r *Reconciler) pullImage(ctx context.Context, image string) error {
	pullCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return r.docker.PullImage(pullCtx, image, r.cfg.RegistryAuth)
}

func (r *Reconciler) newName(pool string, existing map[string]bool) string {
	for {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		name := r.cfg.NamePrefix + "-" + pool + "-" + hex.EncodeToString(b)
		if !existing[name] {
			return name
		}
	}
}

func (r *Reconciler) buildSpec(p Pool, gen, name, token string) ContainerSpec {
	env := []string{
		"RUNNER_TOKEN=" + token,
		"RUNNER_REGISTER_TO=" + r.cfg.Target.String(),
		"RUNNER_NAME=" + name,
		"RUNNER_LABELS=" + normalizeLabels(p.Labels),
		"RUNNER_GROUP=" + p.Group,
		"RUNNER_EPHEMERAL=" + strconv.FormatBool(p.Ephemeral),
		"RUNNER_DISABLE_UPDATE=" + strconv.FormatBool(p.Ephemeral),
		"RUNNER_GRACEFUL_STOP_TIMEOUT=" + strconv.Itoa(p.GracefulStopSeconds),
	}
	if r.cfg.GitHubURL != "" && strings.TrimRight(r.cfg.GitHubURL, "/") != "https://github.com" {
		env = append(env, "GITHUB_URL="+r.cfg.GitHubURL)
	}
	var binds []string
	if p.DockerSocket {
		binds = append(binds, r.cfg.DockerSocket+":/var/run/docker.sock")
	}
	if p.WorkBase != "" {
		dir := path.Join(p.WorkBase, name)
		binds = append(binds, dir+":"+dir)
		env = append(env, "RUNNER_WORKDIR="+dir)
	}
	env = append(env, p.ExtraEnvList()...)

	restart := "unless-stopped"
	if p.Ephemeral {
		restart = "no"
	}
	return ContainerSpec{
		Name:     name,
		Image:    p.EffectiveImage(),
		Hostname: name,
		Env:      env,
		Labels: map[string]string{
			labelManaged:    "true",
			labelName:       name,
			labelGeneration: gen,
			labelEphemeral:  strconv.FormatBool(p.Ephemeral),
			labelTarget:     r.cfg.Target.String(),
			labelPool:       p.Name,
		},
		Binds:         binds,
		RestartPolicy: restart,
		StopTimeout:   p.GracefulStopSeconds + 60,
	}
}

func (r *Reconciler) createRunner(ctx context.Context, p Pool, gen string, existing map[string]bool) error {
	name := r.newName(p.Name, existing)
	token, err := r.github.RegistrationToken(ctx)
	if err != nil {
		return fmt.Errorf("registration token: %w", err)
	}
	spec := r.buildSpec(p, gen, name, token)
	id, err := r.docker.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if err := r.docker.StartContainer(ctx, id); err != nil {
		_ = r.docker.RemoveContainer(ctx, id, true)
		return fmt.Errorf("start %s: %w", name, err)
	}
	existing[name] = true
	r.events.Infof("%s: created (%s)", name, spec.Image)
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
func (r *Reconciler) startDrain(name, id, reason string, gracefulSeconds int) {
	r.dmu.Lock()
	if _, already := r.draining[name]; already {
		r.dmu.Unlock()
		return
	}
	r.draining[name] = drainInfo{Reason: reason, Started: time.Now()}
	r.dmu.Unlock()

	r.events.Infof("%s: draining (%s)", name, reason)
	stopTimeout := time.Duration(gracefulSeconds+60) * time.Second
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
	containers, err := r.listManaged(ctx)
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

func gracefulFor(s Settings, poolName string) int {
	if p, ok := s.Pool(poolName); ok {
		return p.GracefulStopSeconds
	}
	return defaultGracefulStopSeconds
}

// Destroy removes one runner for good and lowers its pool's desired count.
func (r *Reconciler) Destroy(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.findContainer(ctx, name)
	if err != nil {
		return err
	}
	pool := poolOf(c, r.settings.Get())
	s, err := r.settings.Update(func(s *Settings) error {
		if p, ok := s.Pool(pool); ok && p.Count > 0 {
			p.Count--
			s.SetPool(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.startDrain(name, c.ID, "destroyed by operator", gracefulFor(s, pool))
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
	s := r.settings.Get()
	r.startDrain(name, c.ID, "recreate requested", gracefulFor(s, poolOf(c, s)))
	return nil
}

// errPullInProgress is returned when another pull holds the pulling flag.
var errPullInProgress = errors.New("a pull is already in progress")

// poolImages returns the images to pull for one pool, or for every pool when
// poolName is empty.
func (r *Reconciler) poolImages(poolName string) ([]string, error) {
	s := r.settings.Get()
	if poolName == "" {
		return s.DistinctImages(), nil
	}
	p, ok := s.Pool(poolName)
	if !ok {
		return nil, fmt.Errorf("no pool named %q", poolName)
	}
	return []string{p.EffectiveImage()}, nil
}

// PullAndRoll pulls the image of one pool (or of every pool when poolName is
// empty) and waits for it; runners built from an older image are then replaced
// one at a time by the reconcile loop.
func (r *Reconciler) PullAndRoll(ctx context.Context, poolName string) error {
	images, err := r.poolImages(poolName)
	if err != nil {
		return err
	}
	if !r.pulling.CompareAndSwap(false, true) {
		return errPullInProgress
	}
	defer r.pulling.Store(false)
	return r.pullImages(ctx, images)
}

// StartPullAndRoll is PullAndRoll in the background: it reserves the pulling
// flag before returning, so a concurrent request gets errPullInProgress
// instead of a silently dropped pull.
func (r *Reconciler) StartPullAndRoll(poolName string) error {
	images, err := r.poolImages(poolName)
	if err != nil {
		return err
	}
	if !r.pulling.CompareAndSwap(false, true) {
		return errPullInProgress
	}
	go func() {
		defer r.pulling.Store(false)
		_ = r.pullImages(context.Background(), images)
	}()
	return nil
}

func (r *Reconciler) pullImages(ctx context.Context, images []string) error {
	var errs []error
	for _, image := range images {
		if err := r.pull(ctx, image); err != nil {
			r.events.Errorf("pull %s: %v", image, err)
			errs = append(errs, err)
		}
	}
	r.Wake()
	return errors.Join(errs...)
}

// RunnerView is one runner as shown in the UI.
type RunnerView struct {
	Name          string    `json:"name"`
	Pool          string    `json:"pool"`
	ContainerID   string    `json:"container_id"`
	State         string    `json:"state"`
	Health        string    `json:"health"`
	Status        string    `json:"status"`
	Created       time.Time `json:"created"`
	Image         string    `json:"image"`
	Flavor        string    `json:"flavor"`
	RunnerVersion string    `json:"runner_version"`
	Generation    string    `json:"generation"`
	Stale         bool      `json:"stale"`
	Draining      bool      `json:"draining"`
	DrainReason   string    `json:"drain_reason,omitempty"`
	GitHubStatus  string    `json:"github_status"`
	Busy          bool      `json:"busy"`
	Ephemeral     bool      `json:"ephemeral"`
}

// PoolView is one pool with its runners as shown in the UI.
type PoolView struct {
	Pool
	EffectiveImage string       `json:"effective_image"`
	Generation     string       `json:"generation"`
	Flavor         string       `json:"flavor"`
	RunnerVersion  string       `json:"runner_version"`
	Running        int          `json:"running"`
	Busy           int          `json:"busy"`
	Runners        []RunnerView `json:"runners"`
}

// State is everything the UI needs.
type State struct {
	Version             string       `json:"version"`
	Target              string       `json:"target"`
	GitHubURL           string       `json:"github_url"`
	AutoUpdate          bool         `json:"auto_update"`
	UpdateCheckInterval string       `json:"update_check_interval"`
	LatestRunnerVersion string       `json:"latest_runner_version"`
	Runtimes            []Runtime    `json:"runtimes"`
	Pools               []PoolView   `json:"pools"`
	Orphans             []RunnerView `json:"orphans"`
	Events              []Event      `json:"events"`
	Pulling             bool         `json:"pulling"`
	GitHubError         string       `json:"github_error,omitempty"`
	DockerError         string       `json:"docker_error,omitempty"`
	LastPass            time.Time    `json:"last_pass"`
	Now                 time.Time    `json:"now"`
}

// Runtime is a selectable runtime environment.
type Runtime struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// runtimeList is the catalog plus "custom", for the dashboard's dropdown.
func (r *Reconciler) runtimeList() []Runtime {
	out := make([]Runtime, 0, len(r.cfg.Flavors)+1)
	for _, f := range r.cfg.Flavors {
		out = append(out, Runtime{Name: f.Name, Label: f.Label})
	}
	return append(out, Runtime{Name: "custom", Label: "Custom image"})
}

// Snapshot merges Docker and GitHub views for the dashboard (read-only).
func (r *Reconciler) Snapshot(ctx context.Context) (State, error) {
	s := r.settings.Get()
	st := State{
		Version: version, Target: r.cfg.Target.String(), GitHubURL: r.cfg.Target.URL(r.cfg.GitHubURL),
		AutoUpdate: s.AutoUpdate, UpdateCheckInterval: shortDuration(r.cfg.UpdateInterval),
		LatestRunnerVersion: r.latestRunner.Load().(string), Runtimes: r.runtimeList(),
		Events: r.events.List(), Pulling: r.pulling.Load(), GitHubError: r.githubErr.Load().(string),
		LastPass: r.lastPassTime.Load().(time.Time), Now: time.Now(),
		Pools: []PoolView{}, Orphans: []RunnerView{}, // arrays, never null, for the page
	}
	containers, err := r.listManaged(ctx)
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

	views := map[string]*PoolView{}
	images := map[string]ImageInfo{}
	for _, p := range s.Pools {
		img, done := images[p.EffectiveImage()]
		if !done {
			img, _ = r.docker.InspectImage(ctx, p.EffectiveImage())
			images[p.EffectiveImage()] = img
		}
		views[p.Name] = &PoolView{
			Pool: p, EffectiveImage: p.EffectiveImage(), Generation: p.Generation(),
			Flavor: img.Labels[labelFlavor], RunnerVersion: img.Labels[labelRunnerVersion], Runners: []RunnerView{},
		}
	}

	for _, c := range containers {
		if !r.ownsContainer(c) {
			continue
		}
		name := runnerName(c)
		v := RunnerView{
			Name: name, Pool: poolOf(c, s), ContainerID: c.ID, State: c.State, Health: c.Health, Status: c.Status, Created: c.Created,
			Image: c.Image, Flavor: c.Labels[labelFlavor], RunnerVersion: c.Labels[labelRunnerVersion],
			Generation: c.Labels[labelGeneration], Ephemeral: isEphemeral(c), GitHubStatus: "unknown",
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
		pv, ok := views[v.Pool]
		if !ok {
			v.Stale = true
			st.Orphans = append(st.Orphans, v)
			continue
		}
		v.Stale = isStale(c, pv.Generation, images[pv.EffectiveImage].ID)
		if c.State == "running" && !v.Draining {
			pv.Running++
		}
		if v.Busy {
			pv.Busy++
		}
		pv.Runners = append(pv.Runners, v)
	}
	for _, p := range s.Pools {
		pv := views[p.Name]
		sort.Slice(pv.Runners, func(i, j int) bool { return pv.Runners[i].Created.Before(pv.Runners[j].Created) })
		st.Pools = append(st.Pools, *pv)
	}
	sort.Slice(st.Orphans, func(i, j int) bool { return st.Orphans[i].Created.Before(st.Orphans[j].Created) })
	return st, nil
}

// shortDuration renders a duration without zero units: 1h, 15m, 1h30m, 1m30s.
func shortDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	h, m, sec := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if sec > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", sec)
	}
	return b.String()
}
