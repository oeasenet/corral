package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

//go:embed templates/*
var templateFS embed.FS

type Config struct {
	PATs map[string]string `json:"pats"`
}

type KMSServer struct {
	config     *Config
	mu         sync.RWMutex
	stats      *Statistics
	startTime  time.Time
	httpClient *http.Client
}

type Statistics struct {
	TotalRequests      int64            `json:"total_requests"`
	SuccessfulRequests int64            `json:"successful_requests"`
	FailedRequests     int64            `json:"failed_requests"`
	RequestsByOrg      map[string]int64 `json:"requests_by_org"`
	RequestsByRepo     map[string]int64 `json:"requests_by_repo"`
	LastRequestTime    time.Time        `json:"last_request_time"`
	mu                 sync.RWMutex
}

type TokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func NewKMSServer() *KMSServer {
	return &KMSServer{
		config: &Config{
			PATs: make(map[string]string),
		},
		stats: &Statistics{
			RequestsByOrg:  make(map[string]int64),
			RequestsByRepo: make(map[string]int64),
		},
		startTime: time.Now(),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (s *KMSServer) LoadConfig() error {
	configFile := "config.json"
	if data, err := os.ReadFile(configFile); err == nil && len(data) > 0 {
		var fileConfig map[string]string
		if err := json.Unmarshal(data, &fileConfig); err == nil {
			for k, v := range fileConfig {
				s.config.PATs[k] = v
			}
			log.Printf("Loaded %d PATs from config.json", len(fileConfig))
		}
	}

	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "PAT_") {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) == 2 {
				patLabel := strings.TrimPrefix(parts[0], "PAT_")
				if patLabel != "" {
					s.config.PATs[patLabel] = strings.TrimSpace(parts[1])
					log.Printf("Loaded PAT for %s from environment", patLabel)
				}
			}
		}
	}

	if len(s.config.PATs) == 0 {
		return fmt.Errorf("no PATs configured. Please set PAT_* environment variables or add them to config.json")
	}

	return nil
}

func (s *KMSServer) getGitHubToken(url, owner string) (string, error) {
	s.mu.RLock()
	pat, exists := s.config.PATs[owner]
	s.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("no PAT configured for %s", owner)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", pat))
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub API error: %s (status: %d)", string(body), resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	token, ok := result["token"].(string)
	if !ok {
		return "", fmt.Errorf("invalid response from GitHub API")
	}

	return token, nil
}

func (s *KMSServer) updateStats(owner, repo string, success bool) {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()

	s.stats.TotalRequests++
	if success {
		s.stats.SuccessfulRequests++
	} else {
		s.stats.FailedRequests++
	}

	if owner != "" {
		s.stats.RequestsByOrg[owner]++
	}
	if repo != "" {
		fullRepo := fmt.Sprintf("%s/%s", owner, repo)
		s.stats.RequestsByRepo[fullRepo]++
	}
	s.stats.LastRequestTime = time.Now()
}

func (s *KMSServer) handleRepoRegistrationToken(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	owner := vars["owner"]
	repo := vars["repo"]

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners/registration-token", owner, repo)

	token, err := s.getGitHubToken(url, owner)
	s.updateStats(owner, repo, err == nil)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, token)
}

func (s *KMSServer) handleRepoRemoveToken(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	owner := vars["owner"]
	repo := vars["repo"]

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners/remove-token", owner, repo)

	token, err := s.getGitHubToken(url, owner)
	s.updateStats(owner, repo, err == nil)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, token)
}

func (s *KMSServer) handleOrgRegistrationToken(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	org := vars["org"]

	url := fmt.Sprintf("https://api.github.com/orgs/%s/actions/runners/registration-token", org)

	token, err := s.getGitHubToken(url, org)
	s.updateStats(org, "", err == nil)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, token)
}

func (s *KMSServer) handleOrgRemoveToken(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	org := vars["org"]

	url := fmt.Sprintf("https://api.github.com/orgs/%s/actions/runners/remove-token", org)

	token, err := s.getGitHubToken(url, org)
	s.updateStats(org, "", err == nil)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, token)
}

func (s *KMSServer) handleUI(w http.ResponseWriter, r *http.Request) {
	funcMap := template.FuncMap{
		"divf": func(a, b int64) float64 {
			if b == 0 {
				return 0
			}
			return float64(a) / float64(b)
		},
		"mulf": func(a float64, b float64) float64 {
			return a * b
		},
	}
	tmpl := template.Must(template.New("index.html").Funcs(funcMap).ParseFS(templateFS, "templates/index.html"))

	s.stats.mu.RLock()
	stats := *s.stats
	s.stats.mu.RUnlock()

	s.mu.RLock()
	configuredOrgs := make([]string, 0, len(s.config.PATs))
	for org := range s.config.PATs {
		configuredOrgs = append(configuredOrgs, org)
	}
	s.mu.RUnlock()

	data := struct {
		Stats          Statistics
		Uptime         string
		ConfiguredOrgs []string
	}{
		Stats:          stats,
		Uptime:         time.Since(s.startTime).Round(time.Second).String(),
		ConfiguredOrgs: configuredOrgs,
	}

	w.Header().Set("Content-Type", "text/html")
	tmpl.Execute(w, data)
}

func (s *KMSServer) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	s.stats.mu.RLock()
	defer s.stats.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.stats)
}

func (s *KMSServer) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	configuredOrgs := make([]string, 0, len(s.config.PATs))
	for org := range s.config.PATs {
		configuredOrgs = append(configuredOrgs, org)
	}

	response := map[string]interface{}{
		"configured_organizations": configuredOrgs,
		"total_pats":               len(s.config.PATs),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *KMSServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	hasPATs := len(s.config.PATs) > 0
	s.mu.RUnlock()

	status := "healthy"
	if !hasPATs {
		status = "unhealthy"
	}

	response := map[string]interface{}{
		"status":    status,
		"uptime":    time.Since(s.startTime).Seconds(),
		"timestamp": time.Now().Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func main() {
	server := NewKMSServer()

	if err := server.LoadConfig(); err != nil {
		log.Fatal(err)
	}

	router := mux.NewRouter()

	router.HandleFunc("/repo/{owner}/{repo}/registration-token", server.handleRepoRegistrationToken).Methods("GET")
	router.HandleFunc("/repo/{owner}/{repo}/remove-token", server.handleRepoRemoveToken).Methods("GET")
	router.HandleFunc("/{org}/registration-token", server.handleOrgRegistrationToken).Methods("GET")
	router.HandleFunc("/{org}/remove-token", server.handleOrgRemoveToken).Methods("GET")

	router.HandleFunc("/", server.handleUI).Methods("GET")
	router.HandleFunc("/api/stats", server.handleAPIStats).Methods("GET")
	router.HandleFunc("/api/config", server.handleAPIConfig).Methods("GET")
	router.HandleFunc("/health", server.handleHealth).Methods("GET")

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	log.Printf("GitHub Runner KMS Server starting on port %s", port)
	log.Printf("UI available at http://localhost:%s/", port)

	if err := http.ListenAndServe(":"+port, router); err != nil {
		log.Fatal(err)
	}
}
