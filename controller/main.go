// Command controller (corral) runs a fleet of GitHub Actions self-hosted runner
// containers on the local Docker host: it registers them with GitHub, keeps
// the desired number running, replaces outdated or broken ones and removes
// them cleanly, with a small web UI for runtime control.
//
// Required environment: GITHUB_PAT and GITHUB_OWNER (or RUNNER_REGISTER_TO).
// Everything else is optional; see README.md.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := loadControllerConfig(os.Environ())
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	docker, err := NewDockerClient(cfg.DockerHost, cfg.DockerAPIVersion)
	if err != nil {
		log.Fatalf("docker: %v", err)
	}
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 10*time.Second)
	if err := docker.Ping(pingCtx); err != nil {
		log.Fatalf("cannot reach the Docker daemon at %s (is /var/run/docker.sock mounted into this container?): %v", cfg.DockerHost, err)
	}
	cancelPing()

	store, err := LoadSettings(filepath.Join(cfg.DataDir, "settings.json"), SettingsFromEnv(os.Environ()))
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	flavors, err := LoadFlavors(cfg.FlavorsDir)
	if err != nil {
		log.Printf("flavor catalog: skipping unusable entries: %v", err)
	}
	cfg.Flavors = flavors
	if len(flavors) == 0 {
		log.Printf("flavor catalog: nothing in %s; only custom images can be configured (set FLAVORS_DIR)", cfg.FlavorsDir)
	} else {
		log.Printf("flavor catalog: %d flavors from %s", len(flavors), cfg.FlavorsDir)
	}

	github := NewGitHubClient(cfg.GitHubAPI, cfg.PAT, cfg.Target)
	events := NewEventLog(200)
	rec := NewReconciler(docker, github, store, cfg.ControllerConfig, events)
	srv, err := NewServer(rec, cfg.AdminPassword)
	if err != nil {
		log.Fatal(err)
	}

	settings := store.Get()
	events.Infof("corral %s: target %s, %d pool(s), %d runners desired, automatic updates %v (every %s)",
		version, cfg.Target, len(settings.Pools), settings.TotalCount(), settings.AutoUpdate, cfg.UpdateInterval)
	for _, p := range settings.Pools {
		events.Infof("pool %s: runtime %s, image %s, %d runners", p.Name, p.Runtime, p.EffectiveImage(), p.Count)
	}
	if cfg.AdminPassword == "" {
		log.Print("dashboard authentication is off (set ADMIN_PASSWORD to require a login)")
	} else {
		log.Print("dashboard authentication is on (user: admin)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go rec.Run(ctx)

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	log.Printf("dashboard listening on %s", httpServer.Addr)

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	case <-ctx.Done():
		log.Print("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}

	// Runners keep working without the controller; give in-flight drains a
	// moment, the next start reconciles whatever is left.
	done := make(chan struct{})
	go func() { rec.WaitForDrains(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Print("drains still in progress; they will be picked up on the next start")
	}
}
