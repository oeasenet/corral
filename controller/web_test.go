package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, password string) (http.Handler, *Reconciler, *fakeDockerAPI, *fakeGitHubAPI) {
	t.Helper()
	r, d, g := newTestReconciler(t, baseSettings(2))
	srv, err := NewServer(r, password)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv.Handler(), r, d, g
}

func request(t *testing.T, h http.Handler, method, path, body string, auth string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.SetBasicAuth("admin", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealth_IsPublicEvenWithPassword(t *testing.T) {
	h, _, _, _ := newTestServer(t, "secret")
	rec := request(t, h, http.MethodGet, "/health", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status"`) {
		t.Errorf("health: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_ProtectsDashboardAndAPI(t *testing.T) {
	h, _, _, _ := newTestServer(t, "secret")
	for _, path := range []string{"/", "/api/state"} {
		rec := request(t, h, http.MethodGet, path, "", "")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
			t.Errorf("%s without credentials: %d %q", path, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
		if rec := request(t, h, http.MethodGet, path, "", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with wrong password: %d", path, rec.Code)
		}
		if rec := request(t, h, http.MethodGet, path, "", "secret"); rec.Code != http.StatusOK {
			t.Errorf("%s with the right password: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := request(t, h, http.MethodPost, "/api/scale", `{"delta":1}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("mutations must be protected: %d", rec.Code)
	}
}

func TestAuth_DisabledWithoutPassword(t *testing.T) {
	h, _, _, _ := newTestServer(t, "")
	if rec := request(t, h, http.MethodGet, "/api/state", "", ""); rec.Code != http.StatusOK {
		t.Errorf("no password configured means open access: %d", rec.Code)
	}
}

func TestAPIState_ReturnsSnapshot(t *testing.T) {
	h, r, d, g := newTestServer(t, "")
	d.add("oease-one00000", "running", "healthy", r.settings.Get().Generation(), "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "oease-one00000", Status: "online", Busy: true}}

	rec := request(t, h, http.MethodGet, "/api/state", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var st State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if st.Target != "oeasenet" || st.Settings.Count != 2 || len(st.Runners) != 1 || st.Runners[0].GitHubStatus != "online" || !st.Runners[0].Busy {
		t.Errorf("state: %+v", st)
	}
}

func TestAPISettings_UpdatesValidatesAndWakes(t *testing.T) {
	h, r, _, _ := newTestServer(t, "")
	body := `{"count":3,"labels":"docker, gpu","group":"prod","image":"ghcr.io/oeasenet/gha-docker-runner/runner:main","ephemeral":false,"docker_socket":true,"work_base":"","extra_env":"","graceful_stop_seconds":600}`
	rec := request(t, h, http.MethodPut, "/api/settings", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	s := r.settings.Get()
	if s.Count != 3 || s.Labels != "docker,gpu" || s.Group != "prod" || s.GracefulStopSeconds != 600 || !strings.HasSuffix(s.Image, ":main") {
		t.Errorf("settings not applied: %+v", s)
	}
	select {
	case <-r.wake:
	default:
		t.Error("a settings change must wake the reconciler")
	}

	for _, bad := range []string{
		`{"count":-1,"image":"x"}`,
		`{"count":1,"image":""}`,
		`{"count":1,"image":"x","extra_env":"RUNNER_TOKEN=leak"}`,
		`not json`,
	} {
		rec := request(t, h, http.MethodPut, "/api/settings", bad, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	if r.settings.Get().Count != 3 {
		t.Error("rejected updates must not change settings")
	}
}

func TestAPIScale_AdjustsCountAndClampsAtZero(t *testing.T) {
	h, r, _, _ := newTestServer(t, "")
	if rec := request(t, h, http.MethodPost, "/api/scale", `{"delta":2}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("scale up: %d %s", rec.Code, rec.Body.String())
	}
	if r.settings.Get().Count != 4 {
		t.Errorf("count after +2: %d", r.settings.Get().Count)
	}
	if rec := request(t, h, http.MethodPost, "/api/scale", `{"delta":-10}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("scale down: %d %s", rec.Code, rec.Body.String())
	}
	if r.settings.Get().Count != 0 {
		t.Errorf("count must clamp at 0, got %d", r.settings.Get().Count)
	}
	if rec := request(t, h, http.MethodPost, "/api/scale", `{"count":5}`, ""); rec.Code != http.StatusOK || r.settings.Get().Count != 5 {
		t.Errorf("absolute count: %d %s (count=%d)", rec.Code, rec.Body.String(), r.settings.Get().Count)
	}
}

func TestAPIRunnerActions(t *testing.T) {
	h, r, d, _ := newTestServer(t, "")
	d.add("oease-abc00000", "running", "healthy", r.settings.Get().Generation(), "sha256:current", time.Now())

	if rec := request(t, h, http.MethodPost, "/api/runners/oease-nope0000/destroy", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown runner: %d", rec.Code)
	}
	rec := request(t, h, http.MethodPost, "/api/runners/oease-abc00000/destroy", "", "")
	if rec.Code != http.StatusAccepted {
		t.Errorf("destroy: %d %s", rec.Code, rec.Body.String())
	}
	r.WaitForDrains()
	if r.settings.Get().Count != 1 || d.byName("oease-abc00000") != nil {
		t.Errorf("destroy must drop the count and remove the container: count=%d names=%v", r.settings.Get().Count, d.names())
	}

	d.add("oease-def00000", "running", "healthy", r.settings.Get().Generation(), "sha256:current", time.Now())
	if rec := request(t, h, http.MethodPost, "/api/runners/oease-def00000/recreate", "", ""); rec.Code != http.StatusAccepted {
		t.Errorf("recreate: %d %s", rec.Code, rec.Body.String())
	}
	r.WaitForDrains()
	if d.byName("oease-def00000") != nil || r.settings.Get().Count != 1 {
		t.Errorf("recreate must remove the container and keep the count: names=%v count=%d", d.names(), r.settings.Get().Count)
	}
}

func TestAPIPull_RunsInBackground(t *testing.T) {
	h, _, d, _ := newTestServer(t, "")
	rec := request(t, h, http.MethodPost, "/api/pull", "", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		n := len(d.pulled)
		d.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("image was never pulled")
}

func TestAPI_RejectsWrongMethods(t *testing.T) {
	h, _, _, _ := newTestServer(t, "")
	if rec := request(t, h, http.MethodGet, "/api/pull", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/pull: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodGet, "/api/runners/x/destroy", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET destroy: %d", rec.Code)
	}
}

func TestDashboard_RendersRunnersAndSettings(t *testing.T) {
	h, r, d, g := newTestServer(t, "")
	d.add("oease-ui000000", "running", "healthy", r.settings.Get().Generation(), "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "oease-ui000000", Status: "online"}}

	rec := request(t, h, http.MethodGet, "/", "", "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("dashboard: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{"oease", "oease-ui000000", "oeasenet", DefaultRunnerImage} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}
