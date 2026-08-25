package main

import (
	"context"
	"time"
)

// maybeCheckForUpdates starts one background image check when automatic
// updates are on and the interval has elapsed. Called at the end of a pass.
func (r *Reconciler) maybeCheckForUpdates(s Settings) {
	if !s.AutoUpdate || r.cfg.UpdateInterval <= 0 || len(s.Pools) == 0 {
		return
	}
	last := r.lastUpdateCheck.Load().(time.Time)
	if !last.IsZero() && r.now().Sub(last) < r.cfg.UpdateInterval {
		return
	}
	if !r.pulling.CompareAndSwap(false, true) {
		return // a manual pull is running; try again on the next pass
	}
	r.lastUpdateCheck.Store(r.now())
	r.updateWG.Add(1)
	go func() {
		defer r.updateWG.Done()
		defer r.pulling.Store(false)
		r.checkForUpdates(context.Background(), s)
	}()
}

// WaitForUpdateCheck blocks until a background image check has finished (tests).
func (r *Reconciler) WaitForUpdateCheck() { r.updateWG.Wait() }

// checkForUpdates records the latest actions/runner release, pulls every pool
// image and announces the ones that changed; the reconcile loop then replaces
// outdated runners as they go idle. CI builds the images; nothing is built here.
func (r *Reconciler) checkForUpdates(ctx context.Context, s Settings) {
	if v, err := r.github.LatestRunnerVersion(ctx); err != nil {
		r.events.Warnf("update check: cannot read the latest actions/runner release: %v", err)
	} else if v != r.latestRunner.Load().(string) {
		r.latestRunner.Store(v)
		r.events.Infof("latest actions/runner release: %s", v)
	}
	changed := 0
	for _, image := range s.DistinctImages() {
		before, _ := r.docker.InspectImage(ctx, image)
		if err := r.pullImage(ctx, image); err != nil {
			r.events.Warnf("update check: cannot pull %s (is it in a registry the host can reach?): %v", image, err)
			continue
		}
		after, _ := r.docker.InspectImage(ctx, image)
		if after.ID != "" && after.ID != before.ID {
			changed++
			r.events.Infof("%s: new image %s (runner %s); outdated runners are replaced as they go idle", image, shortID(after.ID), after.Labels[labelRunnerVersion])
		}
	}
	if changed > 0 {
		r.Wake()
	}
}

// shortID abbreviates a sha256 image id for messages.
func shortID(s string) string {
	if i := len("sha256:"); len(s) > i+12 && s[:i] == "sha256:" {
		return s[i : i+12]
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
