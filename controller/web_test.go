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
	req := httptest.NewRequest(method, path, strings.NewReader(body))
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
	for _, m := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/pools/ubuntu/scale", `{"delta":1}`},
		{http.MethodPut, "/api/pools/ubuntu", `{"count":1}`},
		{http.MethodDelete, "/api/pools/ubuntu", ""},
		{http.MethodPut, "/api/settings", `{"auto_update":false}`},
		{http.MethodPost, "/api/pull", ""},
	} {
		if rec := request(t, h, m.method, m.path, m.body, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s must be protected: %d", m.method, m.path, rec.Code)
		}
	}
}

func TestAuth_DisabledWithoutPassword(t *testing.T) {
	h, _, _, _ := newTestServer(t, "")
	if rec := request(t, h, http.MethodGet, "/api/state", "", ""); rec.Code != http.StatusOK {
		t.Errorf("no password configured means open access: %d", rec.Code)
	}
}

func TestAPIState_ReturnsPoolsAndRunners(t *testing.T) {
	h, r, d, g := newTestServer(t, "")
	d.imageLabels[DefaultRunnerImage] = map[string]string{labelFlavor: "ubuntu", labelRunnerVersion: "2.336.0"}
	d.addInPool("ubuntu", "corral-ubuntu-000001", "running", "healthy", gen(r.settings.Get()), "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-ubuntu-000001", Status: "online", Busy: true}}

	rec := request(t, h, http.MethodGet, "/api/state", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var st State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if st.Target != "example-org" || !st.AutoUpdate || st.UpdateCheckInterval == "" || len(st.Runtimes) != 4 {
		t.Errorf("top level: %+v", st)
	}
	if len(st.Pools) != 1 || st.Pools[0].Name != "ubuntu" || st.Pools[0].Count != 2 || st.Pools[0].RunnerVersion != "2.336.0" || st.Pools[0].Flavor != "ubuntu" {
		t.Fatalf("pools: %+v", st.Pools)
	}
	if rs := st.Pools[0].Runners; len(rs) != 1 || rs[0].GitHubStatus != "online" || !rs[0].Busy || rs[0].Pool != "ubuntu" {
		t.Errorf("runners: %+v", rs)
	}
}

func TestAPISettings_TogglesAutoUpdate(t *testing.T) {
	h, r, _, _ := newTestServer(t, "")
	if rec := request(t, h, http.MethodPut, "/api/settings", `{"auto_update":false}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if r.settings.Get().AutoUpdate {
		t.Error("auto_update should be off")
	}
	if rec := request(t, h, http.MethodPut, "/api/settings", `{"pools":[]}`, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("pools cannot be replaced through /api/settings: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodPut, "/api/settings", `nope`, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: %d", rec.Code)
	}
}

func TestAPIPools_CreateUpdateValidateDelete(t *testing.T) {
	h, r, _, _ := newTestServer(t, "")

	rec := request(t, h, http.MethodPut, "/api/pools/debian", `{"runtime":"debian","count":1,"labels":"deb, arm"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	p, ok := r.settings.Get().Pool("debian")
	if !ok || p.Runtime != "debian" || p.Count != 1 || p.Labels != "deb,arm" || !p.DockerSocket || p.GracefulStopSeconds != defaultGracefulStopSeconds {
		t.Errorf("new pools start from defaults and take the given fields: %+v", p)
	}
	select {
	case <-r.wake:
	default:
		t.Error("a pool change must wake the reconciler")
	}

	rec = request(t, h, http.MethodPut, "/api/pools/debian", `{"count":3,"ephemeral":true}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	p, _ = r.settings.Get().Pool("debian")
	if p.Count != 3 || !p.Ephemeral || p.Labels != "deb,arm" || p.Runtime != "debian" {
		t.Errorf("update must overlay the existing pool: %+v", p)
	}

	for _, bad := range []struct{ name, body string }{
		{"debian", `{"count":-1}`},
		{"debian", `{"runtime":"Arch Linux"}`},
		{"debian", `{"extra_env":"RUNNER_TOKEN=x"}`},
		{"debian", `{"name":"other"}`},
		{"debian", `{"bogus":1}`},
		{"Bad%20Name", `{"runtime":"ubuntu"}`},
		{"custom1", `{"runtime":"custom"}`},
	} {
		if rec := request(t, h, http.MethodPut, "/api/pools/"+bad.name, bad.body, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: expected 400, got %d %s", bad.name, bad.body, rec.Code, rec.Body.String())
		}
	}
	if p, _ := r.settings.Get().Pool("debian"); p.Count != 3 {
		t.Error("rejected updates must not change the pool")
	}

	if rec := request(t, h, http.MethodDelete, "/api/pools/nope", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodDelete, "/api/pools/debian", "", ""); rec.Code != http.StatusOK {
		t.Errorf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := r.settings.Get().Pool("debian"); ok {
		t.Error("pool should be gone")
	}
}

func TestAPIScale_PerPoolAndClamped(t *testing.T) {
	h, r, _, _ := newTestServer(t, "")
	if rec := request(t, h, http.MethodPost, "/api/pools/ubuntu/scale", `{"delta":2}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("scale up: %d %s", rec.Code, rec.Body.String())
	}
	if count(r, "ubuntu") != 4 {
		t.Errorf("count after +2: %d", count(r, "ubuntu"))
	}
	if rec := request(t, h, http.MethodPost, "/api/pools/ubuntu/scale", `{"delta":-10}`, ""); rec.Code != http.StatusOK || count(r, "ubuntu") != 0 {
		t.Errorf("count must clamp at 0: %d (count=%d)", rec.Code, count(r, "ubuntu"))
	}
	if rec := request(t, h, http.MethodPost, "/api/pools/ubuntu/scale", `{"count":5}`, ""); rec.Code != http.StatusOK || count(r, "ubuntu") != 5 {
		t.Errorf("absolute count: %d (count=%d)", rec.Code, count(r, "ubuntu"))
	}
	if rec := request(t, h, http.MethodPost, "/api/pools/nope/scale", `{"delta":1}`, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown pool: %d", rec.Code)
	}
}

func TestAPIRunnerActions(t *testing.T) {
	h, r, d, _ := newTestServer(t, "")
	d.addInPool("ubuntu", "corral-ubuntu-abc001", "running", "healthy", gen(r.settings.Get()), "sha256:current", time.Now())

	if rec := request(t, h, http.MethodPost, "/api/runners/corral-nope-000000/destroy", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown runner: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodPost, "/api/runners/corral-ubuntu-abc001/destroy", "", ""); rec.Code != http.StatusAccepted {
		t.Errorf("destroy: %d %s", rec.Code, rec.Body.String())
	}
	r.WaitForDrains()
	if count(r, "ubuntu") != 1 || d.byName("corral-ubuntu-abc001") != nil {
		t.Errorf("destroy must drop the pool count and remove the container: count=%d names=%v", count(r, "ubuntu"), d.names())
	}

	d.addInPool("ubuntu", "corral-ubuntu-def001", "running", "healthy", gen(r.settings.Get()), "sha256:current", time.Now())
	if rec := request(t, h, http.MethodPost, "/api/runners/corral-ubuntu-def001/recreate", "", ""); rec.Code != http.StatusAccepted {
		t.Errorf("recreate: %d %s", rec.Code, rec.Body.String())
	}
	r.WaitForDrains()
	if d.byName("corral-ubuntu-def001") != nil || count(r, "ubuntu") != 1 {
		t.Errorf("recreate must remove the container and keep the count: names=%v count=%d", d.names(), count(r, "ubuntu"))
	}
}

func waitForPulls(t *testing.T, d *fakeDockerAPI, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		got := len(d.pulled)
		d.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d pulls", n)
}

func TestAPIPull_AllAndPerPoolRunInBackground(t *testing.T) {
	h, r, d, _ := newTestServer(t, "")
	if _, err := r.settings.Update(func(s *Settings) error {
		s.SetPool(Pool{Name: "debian", Runtime: "debian", DockerSocket: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := request(t, h, http.MethodPost, "/api/pull", "", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("pull all: %d %s", rec.Code, rec.Body.String())
	}
	waitForPulls(t, d, 2)
	if rec := request(t, h, http.MethodPost, "/api/pools/debian/pull", "", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("pull pool: %d %s", rec.Code, rec.Body.String())
	}
	waitForPulls(t, d, 3)
	if rec := request(t, h, http.MethodPost, "/api/pools/nope/pull", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown pool: %d", rec.Code)
	}
	r.pulling.Store(true)
	if rec := request(t, h, http.MethodPost, "/api/pools/debian/pull", "", ""); rec.Code != http.StatusConflict {
		t.Errorf("a second pull while one runs must be refused with 409, got %d", rec.Code)
	}
	r.pulling.Store(false)
}

func TestAPI_RejectsWrongMethods(t *testing.T) {
	h, _, _, _ := newTestServer(t, "")
	if rec := request(t, h, http.MethodGet, "/api/pull", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/pull: %d", rec.Code)
	}
	if rec := request(t, h, http.MethodGet, "/api/pools/ubuntu/scale", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET scale: %d", rec.Code)
	}
}

func TestDashboard_RendersPoolsAndRunners(t *testing.T) {
	h, r, d, g := newTestServer(t, "")
	d.addInPool("ubuntu", "corral-ubuntu-ui0001", "running", "healthy", gen(r.settings.Get()), "sha256:current", time.Now())
	g.runners = []Runner{{ID: 1, Name: "corral-ubuntu-ui0001", Status: "online"}}

	rec := request(t, h, http.MethodGet, "/", "", "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("dashboard: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{"corral-ubuntu-ui0001", "example-org", DefaultRunnerImage, `"pools"`, "<script>"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "</script><script>alert") {
		t.Error("state JSON must be safely embedded")
	}
}

func TestAPIState_ListsAreNeverNull(t *testing.T) {
	h, _, _, _ := newTestServer(t, "")
	if rec := request(t, h, http.MethodDelete, "/api/pools/ubuntu", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec := request(t, h, http.MethodGet, "/api/state", "", "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"pools", "orphans", "events", "runtimes"} {
		if string(raw[key]) == "null" {
			t.Errorf("%s must be an array, got null", key)
		}
	}
}
