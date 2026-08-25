package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Configuration loading
// ---------------------------------------------------------------------------

func TestLoadConfig_ReadsPATsFromEnvironment(t *testing.T) {
	cfg, err := loadConfig("", []string{"PAT_myorg=ghp_abc ", "PAT_other=ghp_def", "UNRELATED=1", "PAT_=ignored"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.PATs["myorg"]; got != "ghp_abc" {
		t.Errorf("PAT_myorg: got %q, want %q (whitespace trimmed)", got, "ghp_abc")
	}
	if got := cfg.PATs["other"]; got != "ghp_def" {
		t.Errorf("PAT_other: got %q, want %q", got, "ghp_def")
	}
	if len(cfg.PATs) != 2 {
		t.Errorf("expected exactly 2 PATs, got %d: %v", len(cfg.PATs), cfg.PATs)
	}
}

func TestLoadConfig_ReadsPATsFromFileAndEnvironmentOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"fileorg": "ghp_file", "shared": "ghp_from_file"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path, []string{"PAT_shared=ghp_from_env"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PATs["fileorg"] != "ghp_file" {
		t.Errorf("fileorg PAT not loaded from file: %v", cfg.PATs)
	}
	if cfg.PATs["shared"] != "ghp_from_env" {
		t.Errorf("environment must override file, got %q", cfg.PATs["shared"])
	}
}

func TestLoadConfig_MissingFileIsNotAnError(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"), []string{"PAT_org=ghp_x"})
	if err != nil {
		t.Fatalf("missing config file must be tolerated, got: %v", err)
	}
	if cfg.PATs["org"] != "ghp_x" {
		t.Errorf("env PAT not loaded: %v", cfg.PATs)
	}
}

func TestLoadConfig_MalformedFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, []string{"PAT_org=ghp_x"}); err == nil {
		t.Fatal("malformed config.json must fail loudly instead of being silently ignored")
	}
}

func TestLoadConfig_NoPATsIsAnError(t *testing.T) {
	if _, err := loadConfig("", []string{"HOME=/root"}); err == nil {
		t.Fatal("expected an error when no PATs are configured")
	}
}

func TestLoadConfig_ReadsAuthTokenAndGitHubAPIURL(t *testing.T) {
	cfg, err := loadConfig("", []string{"PAT_org=ghp_x", "KMS_AUTH_TOKEN=s3cret", "GITHUB_API_URL=https://ghe.example.com/api/v3/"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuthToken != "s3cret" {
		t.Errorf("AuthToken: got %q", cfg.AuthToken)
	}
	if cfg.GitHubAPI != "https://ghe.example.com/api/v3" {
		t.Errorf("GitHubAPI should be trimmed of trailing slash, got %q", cfg.GitHubAPI)
	}
}

func TestLoadConfig_DefaultsToPublicGitHubAPI(t *testing.T) {
	cfg, err := loadConfig("", []string{"PAT_org=ghp_x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GitHubAPI != "https://api.github.com" {
		t.Errorf("GitHubAPI default: got %q", cfg.GitHubAPI)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// fakeGitHub records the last request it received and answers with a canned
// registration token. Set status to force an error response.
type fakeGitHub struct {
	*httptest.Server
	status        atomic.Int32
	lastPath      string
	lastMethod    string
	lastAuth      string
	lastAccept    string
	lastAPIVer    string
	requestsSeen  atomic.Int32
	responseToken string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{responseToken: "AAAAREGISTRATIONTOKEN"}
	f.status.Store(http.StatusCreated)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requestsSeen.Add(1)
		f.lastPath = r.URL.Path
		f.lastMethod = r.Method
		f.lastAuth = r.Header.Get("Authorization")
		f.lastAccept = r.Header.Get("Accept")
		f.lastAPIVer = r.Header.Get("X-GitHub-Api-Version")
		status := int(f.status.Load())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusCreated {
			_, _ = io.WriteString(w, `{"token":"`+f.responseToken+`","expires_at":"2026-08-24T12:00:00.000Z"}`)
			return
		}
		_, _ = io.WriteString(w, `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func newTestHandler(t *testing.T, gh *fakeGitHub, authToken string) http.Handler {
	t.Helper()
	srv, err := newServer(Config{
		PATs:      map[string]string{"myorg": "ghp_myorgpat", "someone": "ghp_someonepat"},
		AuthToken: authToken,
		GitHubAPI: gh.URL,
		Version:   "test",
	})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return srv.Handler()
}

func do(t *testing.T, h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Token endpoints
// ---------------------------------------------------------------------------

func TestOrgRegistrationToken_ProxiesToGitHubWithConfiguredPAT(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/myorg/registration-token", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "AAAAREGISTRATIONTOKEN" {
		t.Errorf("body: got %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type: got %q", ct)
	}
	if gh.lastMethod != http.MethodPost || gh.lastPath != "/orgs/myorg/actions/runners/registration-token" {
		t.Errorf("GitHub call: got %s %s", gh.lastMethod, gh.lastPath)
	}
	if gh.lastAuth != "Bearer ghp_myorgpat" {
		t.Errorf("Authorization header: got %q", gh.lastAuth)
	}
	if gh.lastAccept != "application/vnd.github+json" {
		t.Errorf("Accept header: got %q", gh.lastAccept)
	}
	if gh.lastAPIVer == "" {
		t.Error("X-GitHub-Api-Version header must be sent")
	}
}

func TestOrgRemoveToken_CallsRemoveTokenEndpoint(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/myorg/remove-token", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %q", rec.Code, rec.Body.String())
	}
	if gh.lastPath != "/orgs/myorg/actions/runners/remove-token" {
		t.Errorf("GitHub path: got %q", gh.lastPath)
	}
}

func TestRepoRegistrationToken_UsesOwnerPAT(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/repo/someone/my.repo-1/registration-token", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %q", rec.Code, rec.Body.String())
	}
	if gh.lastPath != "/repos/someone/my.repo-1/actions/runners/registration-token" {
		t.Errorf("GitHub path: got %q", gh.lastPath)
	}
	if gh.lastAuth != "Bearer ghp_someonepat" {
		t.Errorf("Authorization header: got %q", gh.lastAuth)
	}
}

func TestRepoRemoveToken_CallsRemoveTokenEndpoint(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/repo/someone/repo/remove-token", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %q", rec.Code, rec.Body.String())
	}
	if gh.lastPath != "/repos/someone/repo/actions/runners/remove-token" {
		t.Errorf("GitHub path: got %q", gh.lastPath)
	}
}

func TestUnknownOwner_Returns404WithoutCallingGitHub(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/unknown-org/registration-token", nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if gh.requestsSeen.Load() != 0 {
		t.Error("GitHub must not be called for an owner without a configured PAT")
	}
}

func TestGitHubFailure_Returns502WithoutLeakingPAT(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.status.Store(http.StatusUnauthorized)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/myorg/registration-token", nil)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ghp_myorgpat") {
		t.Error("response leaked the PAT")
	}
	if !strings.Contains(rec.Body.String(), "401") {
		t.Errorf("response should mention GitHub's status code, got %q", rec.Body.String())
	}
}

func TestInvalidRepoName_Returns400(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/repo/someone/bad%20name/registration-token", nil)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	if gh.requestsSeen.Load() != 0 {
		t.Error("GitHub must not be called with an invalid repository name")
	}
}

func TestTokenEndpoints_OnlyAcceptGET(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodPost, "/myorg/registration-token", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want 405", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Shared-secret authentication
// ---------------------------------------------------------------------------

func TestAuthToken_RequiredForTokenEndpointsWhenConfigured(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "s3cret")

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"wrong scheme", "Token s3cret", http.StatusUnauthorized},
		{"correct token", "Bearer s3cret", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.header != "" {
				headers["Authorization"] = tc.header
			}
			rec := do(t, h, http.MethodGet, "/myorg/registration-token", headers)
			if rec.Code != tc.want {
				t.Errorf("status: got %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if gh.requestsSeen.Load() != 1 {
		t.Errorf("GitHub should only have been called for the authenticated request, saw %d calls", gh.requestsSeen.Load())
	}
}

func TestAuthToken_AlsoProtectsRepoEndpoints(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "s3cret")

	rec := do(t, h, http.MethodGet, "/repo/someone/repo/registration-token", nil)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestAuthToken_DoesNotGateMonitoringEndpoints(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "s3cret")

	for _, path := range []string{"/", "/health", "/api/stats", "/api/config"} {
		rec := do(t, h, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200", path, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Monitoring endpoints
// ---------------------------------------------------------------------------

func TestStats_TrackSuccessesAndFailuresPerOrgAndRepo(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	do(t, h, http.MethodGet, "/myorg/registration-token", nil)
	do(t, h, http.MethodGet, "/repo/someone/repo/registration-token", nil)
	do(t, h, http.MethodGet, "/unknown-org/registration-token", nil)

	rec := do(t, h, http.MethodGet, "/api/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	var stats struct {
		Total          int64            `json:"total_requests"`
		Successful     int64            `json:"successful_requests"`
		Failed         int64            `json:"failed_requests"`
		RequestsByOrg  map[string]int64 `json:"requests_by_org"`
		RequestsByRepo map[string]int64 `json:"requests_by_repo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, rec.Body.String())
	}
	if stats.Total != 3 || stats.Successful != 2 || stats.Failed != 1 {
		t.Errorf("totals: got total=%d ok=%d failed=%d", stats.Total, stats.Successful, stats.Failed)
	}
	if stats.RequestsByOrg["myorg"] != 1 || stats.RequestsByOrg["someone"] != 1 || stats.RequestsByOrg["unknown-org"] != 1 {
		t.Errorf("requests_by_org: %v", stats.RequestsByOrg)
	}
	if stats.RequestsByRepo["someone/repo"] != 1 {
		t.Errorf("requests_by_repo: %v", stats.RequestsByRepo)
	}
}

func TestHealth_ReportsHealthyWithVersion(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "healthy" {
		t.Errorf("status field: got %v", body["status"])
	}
	if body["version"] != "test" {
		t.Errorf("version field: got %v", body["version"])
	}
}

func TestAPIConfig_ListsOwnersButNeverSecrets(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "s3cret")

	rec := do(t, h, http.MethodGet, "/api/config", nil)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(body, `"myorg"`) || !strings.Contains(body, `"someone"`) {
		t.Errorf("configured owners missing from %s", body)
	}
	for _, secret := range []string{"ghp_myorgpat", "ghp_someonepat", "s3cret"} {
		if strings.Contains(body, secret) {
			t.Errorf("/api/config leaked secret %q", secret)
		}
	}
	if !strings.Contains(body, `"auth_required":true`) {
		t.Errorf("/api/config should advertise that auth is required: %s", body)
	}
}

func TestDashboard_RendersConfiguredOwners(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	rec := do(t, h, http.MethodGet, "/", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type: got %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"oease", "myorg", "someone"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "ghp_") {
		t.Error("dashboard leaked a PAT")
	}
}

func TestUnknownPath_Returns404(t *testing.T) {
	gh := newFakeGitHub(t)
	h := newTestHandler(t, gh, "")

	for _, path := range []string{"/nope", "/myorg", "/myorg/something-else", "/repo/only-owner/registration-token"} {
		rec := do(t, h, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", path, rec.Code)
		}
	}
	if gh.requestsSeen.Load() != 0 {
		t.Error("GitHub must not be called for unknown paths")
	}
}
