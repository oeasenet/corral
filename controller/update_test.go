package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func autoUpdateFixture(t *testing.T) (*Reconciler, *fakeDockerAPI, *fakeGitHubAPI, *time.Time) {
	t.Helper()
	s := twoPoolSettings()
	r, d, g := newTestReconciler(t, s)
	r.cfg.UpdateInterval = time.Hour
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	d.imageIDs[RunnerImageRepo+":debian"] = "sha256:debian"
	d.addInPool("ubuntu", "corral-ubuntu-000001", "running", "healthy", gen(s), "sha256:current", now.Add(-time.Hour))
	d.addInPool("debian", "corral-debian-000001", "running", "healthy", mustPool(s, "debian").Generation(), "sha256:debian", now.Add(-2*time.Hour))
	d.addInPool("debian", "corral-debian-000002", "running", "healthy", mustPool(s, "debian").Generation(), "sha256:debian", now.Add(-time.Hour))
	g.runners = []Runner{
		{ID: 1, Name: "corral-ubuntu-000001", Status: "online"},
		{ID: 2, Name: "corral-debian-000001", Status: "online"},
		{ID: 3, Name: "corral-debian-000002", Status: "online"},
	}
	g.latest = "2.337.0"
	return r, d, g, &now
}

func TestAutoUpdate_PullsEachImageOnceAndRollsOutdatedRunners(t *testing.T) {
	r, d, _, now := autoUpdateFixture(t)

	reconcile(t, r) // first pass: never checked before, so the check is due
	r.WaitForUpdateCheck()
	if len(d.pulled) != 2 || d.pulled[0] != RunnerImageRepo+":debian" || d.pulled[1] != RunnerImageRepo+":ubuntu" {
		t.Fatalf("both pool images should be pulled once: %v", d.pulled)
	}
	if got := r.latestRunner.Load().(string); got != "2.337.0" {
		t.Errorf("latest runner version should be recorded, got %q", got)
	}
	newImage := false
	for _, e := range r.events.List() {
		if strings.Contains(e.Message, "new image") {
			newImage = true
		}
	}
	if !newImage {
		t.Error("a changed image must be announced in the event log")
	}
	if len(d.specs) != 0 {
		t.Errorf("the check itself must not create containers, got %d", len(d.specs))
	}

	// The fake pull changed both image ids, so each pool rolls one runner per pass.
	reconcile(t, r)
	r.WaitForDrains()
	if len(d.stops) != 2 {
		t.Fatalf("one outdated runner per pool should drain, got %+v", d.stops)
	}
	for i := 0; i < 4; i++ {
		reconcile(t, r)
		r.WaitForDrains()
	}
	for _, name := range d.names() {
		if c := d.byName(name); c.ImageID != "sha256:pulled" {
			t.Errorf("%s still runs the old image %s", name, c.ImageID)
		}
	}

	// Not due again until the interval has elapsed.
	d.pulled = nil
	*now = now.Add(30 * time.Minute)
	reconcile(t, r)
	r.WaitForUpdateCheck()
	if len(d.pulled) != 0 {
		t.Errorf("no check inside the interval, got pulls %v", d.pulled)
	}
	*now = now.Add(31 * time.Minute)
	reconcile(t, r)
	r.WaitForUpdateCheck()
	if len(d.pulled) != 2 {
		t.Errorf("check due again after the interval, got pulls %v", d.pulled)
	}
}

func TestAutoUpdate_ToggleAndZeroIntervalDisableChecks(t *testing.T) {
	r, d, _, _ := autoUpdateFixture(t)
	if _, err := r.settings.Update(func(s *Settings) error { s.AutoUpdate = false; return nil }); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	r.WaitForUpdateCheck()
	if len(d.pulled) != 0 {
		t.Errorf("auto_update=false must not pull, got %v", d.pulled)
	}

	r2, d2, _, _ := autoUpdateFixture(t)
	r2.cfg.UpdateInterval = 0
	reconcile(t, r2)
	r2.WaitForUpdateCheck()
	if len(d2.pulled) != 0 {
		t.Errorf("UPDATE_CHECK_INTERVAL=0 must not pull, got %v", d2.pulled)
	}
}

func TestAutoUpdate_WaitsWhileAManualPullRuns(t *testing.T) {
	r, d, _, _ := autoUpdateFixture(t)
	r.pulling.Store(true)
	reconcile(t, r)
	r.WaitForUpdateCheck()
	if len(d.pulled) != 0 {
		t.Errorf("no concurrent pulls, got %v", d.pulled)
	}
	if !r.lastUpdateCheck.Load().(time.Time).IsZero() {
		t.Error("a skipped check must not count as done")
	}
	r.pulling.Store(false)
	reconcile(t, r)
	r.WaitForUpdateCheck()
	if len(d.pulled) != 2 {
		t.Errorf("check should run once the manual pull finished, got %v", d.pulled)
	}
}

func TestAutoUpdate_LatestReleaseLookupFailureIsOnlyAWarning(t *testing.T) {
	r, d, g, _ := autoUpdateFixture(t)
	g.latestErr = context.DeadlineExceeded
	reconcile(t, r)
	r.WaitForUpdateCheck()
	if len(d.pulled) != 2 {
		t.Errorf("images are still checked when the release lookup fails, got %v", d.pulled)
	}
	if r.latestRunner.Load().(string) != "" {
		t.Error("no version must be recorded on failure")
	}
}
