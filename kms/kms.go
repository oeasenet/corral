// Command kms is a tiny Key Management Service for GitHub Actions self-hosted
// runners. It keeps GitHub Personal Access Tokens (PATs) out of runner
// containers by exchanging them for short-lived runner registration/removal
// tokens on the runners' behalf.
//
// Configuration (environment):
//
//	PAT_<owner>       PAT able to manage self-hosted runners for <owner>
//	                  (a GitHub organization or user). Repeat per owner.
//	CONFIG_FILE       optional JSON file {"owner": "pat", ...} (default config.json)
//	KMS_AUTH_TOKEN    optional shared secret; when set, token endpoints require
//	                  "Authorization: Bearer <token>" (strongly recommended)
//	GITHUB_API_URL    GitHub REST API base URL (default https://api.github.com)
//	PORT              listen port (default 3000)
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed templates/index.html
var templateFS embed.FS

// version is injected at build time: -ldflags "-X main.version=<tag>".
var version = "dev"

const (
	defaultGitHubAPI  = "https://api.github.com"
	defaultConfigFile = "config.json"
	defaultPort       = "3000"
	githubAPIVersion  = "2022-11-28"
	shutdownTimeout   = 15 * time.Second
)

var (
	validOwner = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	validRepo  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Config is the fully resolved service configuration.
type Config struct {
	// PATs maps a GitHub owner (organization or user login) to the PAT used
	// to mint runner tokens for it.
	PATs map[string]string
	// AuthToken, when non-empty, is required as a Bearer token on the
	// token-issuing endpoints.
	AuthToken string
	// GitHubAPI is the REST API base URL without a trailing slash.
	GitHubAPI string
	// Version is reported by /health and the dashboard.
	Version string
}

// loadConfig builds a Config from an optional JSON file and the given
// environment (os.Environ() format). Environment values override the file.
func loadConfig(configPath string, environ []string) (Config, error) {
	cfg := Config{PATs: map[string]string{}, GitHubAPI: defaultGitHubAPI}

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// The file is optional.
		case err != nil:
			return Config{}, fmt.Errorf("read %s: %w", configPath, err)
		case len(bytes.TrimSpace(data)) > 0:
			var fromFile map[string]string
			if err := json.Unmarshal(data, &fromFile); err != nil {
				return Config{}, fmt.Errorf("parse %s: %w", configPath, err)
			}
			for owner, pat := range fromFile {
				if owner != "" && strings.TrimSpace(pat) != "" {
					cfg.PATs[owner] = strings.TrimSpace(pat)
				}
			}
		}
	}

	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(key, "PAT_"):
			if owner := strings.TrimPrefix(key, "PAT_"); owner != "" && value != "" {
				cfg.PATs[owner] = value
			}
		case key == "KMS_AUTH_TOKEN":
			cfg.AuthToken = value
		case key == "GITHUB_API_URL":
			if value != "" {
				cfg.GitHubAPI = strings.TrimRight(value, "/")
			}
		}
	}

	if len(cfg.PATs) == 0 {
		return Config{}, errors.New("no PATs configured: set PAT_<owner> environment variables or provide a config.json")
	}
	return cfg, nil
}

// statsSnapshot is the JSON/template view of the request counters.
type statsSnapshot struct {
	TotalRequests      int64            `json:"total_requests"`
	SuccessfulRequests int64            `json:"successful_requests"`
	FailedRequests     int64            `json:"failed_requests"`
	RequestsByOrg      map[string]int64 `json:"requests_by_org"`
	RequestsByRepo     map[string]int64 `json:"requests_by_repo"`
	LastRequestTime    time.Time        `json:"last_request_time"`
}

type statistics struct {
	mu   sync.Mutex
	data statsSnapshot
}

func (s *statistics) record(owner, repo string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.TotalRequests++
	if ok {
		s.data.SuccessfulRequests++
	} else {
		s.data.FailedRequests++
	}
	s.data.RequestsByOrg[owner]++
	if repo != "" {
		s.data.RequestsByRepo[owner+"/"+repo]++
	}
	s.data.LastRequestTime = time.Now()
}

func (s *statistics) snapshot() statsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.data
	snap.RequestsByOrg = maps.Clone(s.data.RequestsByOrg)
	snap.RequestsByRepo = maps.Clone(s.data.RequestsByRepo)
	return snap
}

// KMSServer exchanges PATs for runner tokens and serves a small dashboard.
type KMSServer struct {
	cfg       Config
	tmpl      *template.Template
	client    *http.Client
	startTime time.Time
	stats     statistics
}

func newServer(cfg Config) (*KMSServer, error) {
	if cfg.GitHubAPI == "" {
		cfg.GitHubAPI = defaultGitHubAPI
	}
	if cfg.Version == "" {
		cfg.Version = version
	}
	tmpl, err := template.ParseFS(templateFS, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}
	return &KMSServer{
		cfg:       cfg,
		tmpl:      tmpl,
		client:    &http.Client{Timeout: 30 * time.Second},
		startTime: time.Now(),
		stats: statistics{data: statsSnapshot{
			RequestsByOrg:  map[string]int64{},
			RequestsByRepo: map[string]int64{},
		}},
	}, nil
}

// Handler returns the HTTP routes of the service.
func (s *KMSServer) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/config", s.handleConfig)

	mux.Handle("GET /{org}/registration-token", s.requireAuth(s.orgToken("registration-token")))
	mux.Handle("GET /{org}/remove-token", s.requireAuth(s.orgToken("remove-token")))
	mux.Handle("GET /repo/{owner}/{repo}/registration-token", s.requireAuth(s.repoToken("registration-token")))
	mux.Handle("GET /repo/{owner}/{repo}/remove-token", s.requireAuth(s.repoToken("remove-token")))

	return logRequests(mux)
}

func (s *KMSServer) owners() []string {
	return slices.Sorted(maps.Keys(s.cfg.PATs))
}

// requireAuth enforces the shared secret on token-issuing endpoints.
func (s *KMSServer) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken != "" {
			presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.AuthToken)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="kms"`)
				http.Error(w, "unauthorized: a valid KMS_AUTH_TOKEN bearer token is required", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *KMSServer) orgToken(kind string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.issueToken(w, r, r.PathValue("org"), "", kind)
	})
}

func (s *KMSServer) repoToken(kind string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.issueToken(w, r, r.PathValue("owner"), r.PathValue("repo"), kind)
	})
}

// issueToken mints a runner token of the given kind ("registration-token" or
// "remove-token") for an organization (repo == "") or a repository.
func (s *KMSServer) issueToken(w http.ResponseWriter, r *http.Request, owner, repo, kind string) {
	if !validOwner.MatchString(owner) || (repo != "" && !validRepo.MatchString(repo)) {
		http.Error(w, "invalid owner or repository name", http.StatusBadRequest)
		return
	}

	pat, ok := s.cfg.PATs[owner]
	if !ok {
		s.stats.record(owner, repo, false)
		http.Error(w, fmt.Sprintf("no PAT configured for %q", owner), http.StatusNotFound)
		return
	}

	apiPath := fmt.Sprintf("/orgs/%s/actions/runners/%s", owner, kind)
	if repo != "" {
		apiPath = fmt.Sprintf("/repos/%s/%s/actions/runners/%s", owner, repo, kind)
	}

	token, err := s.githubToken(r.Context(), pat, apiPath)
	s.stats.record(owner, repo, err == nil)
	if err != nil {
		log.Printf("token request for %s failed: %v", strings.TrimSuffix(owner+"/"+repo, "/"), err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, token)
}

// githubToken calls the GitHub REST API to create a runner token.
func (s *KMSServer) githubToken(ctx context.Context, pat, apiPath string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.GitHubAPI+apiPath, nil)
	if err != nil {
		return "", fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "oease-gha-kms/"+s.cfg.Version)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github api request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read github response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github api returned %d for %s: %s", resp.StatusCode, apiPath, githubErrorMessage(body))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Token == "" {
		return "", errors.New("github api returned an unexpected response body")
	}
	return result.Token, nil
}

// githubErrorMessage extracts the "message" field of a GitHub error body,
// falling back to a truncated raw body.
func githubErrorMessage(body []byte) string {
	var ghErr struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &ghErr); err == nil && ghErr.Message != "" {
		return ghErr.Message
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

func (s *KMSServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Stats          statsSnapshot
		Uptime         string
		ConfiguredOrgs []string
		AuthRequired   bool
		Version        string
		GitHubAPI      string
	}{
		Stats:          s.stats.snapshot(),
		Uptime:         time.Since(s.startTime).Round(time.Second).String(),
		ConfiguredOrgs: s.owners(),
		AuthRequired:   s.cfg.AuthToken != "",
		Version:        s.cfg.Version,
		GitHubAPI:      s.cfg.GitHubAPI,
	}

	var buf bytes.Buffer
	if err := s.tmpl.Execute(&buf, data); err != nil {
		log.Printf("render dashboard: %v", err)
		http.Error(w, "failed to render dashboard", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (s *KMSServer) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.stats.snapshot())
}

func (s *KMSServer) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"configured_owners": s.owners(),
		"total_pats":        len(s.cfg.PATs),
		"auth_required":     s.cfg.AuthToken != "",
		"github_api":        s.cfg.GitHubAPI,
		"version":           s.cfg.Version,
	})
}

func (s *KMSServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"status":         "healthy",
		"version":        s.cfg.Version,
		"uptime_seconds": int64(time.Since(s.startTime).Seconds()),
		"timestamp":      time.Now().Unix(),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json response: %v", err)
	}
}

// statusRecorder captures the status code for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests logs every request except health probes, which would otherwise
// flood the log at container-healthcheck cadence.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

func main() {
	log.SetFlags(log.LstdFlags)

	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = defaultConfigFile
	}
	cfg, err := loadConfig(configPath, os.Environ())
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	cfg.Version = version

	srv, err := newServer(cfg)
	if err != nil {
		log.Fatal(err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("oease GitHub Runner KMS %s listening on %s", version, httpServer.Addr)
	log.Printf("configured owners: %s (github api: %s)", strings.Join(srv.owners(), ", "), cfg.GitHubAPI)
	if cfg.AuthToken == "" {
		log.Print("WARNING: KMS_AUTH_TOKEN is not set; token endpoints are unauthenticated. Only expose this service on a trusted network.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case err := <-errCh:
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
		log.Print("shutdown signal received, draining connections")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}
}
