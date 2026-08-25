package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	org, err := ParseTarget("oeasenet")
	if err != nil || org.Owner != "oeasenet" || org.Repo != "" {
		t.Errorf("org: %+v %v", org, err)
	}
	if org.RunnersPath() != "/orgs/oeasenet/actions/runners" || org.URL("https://github.com") != "https://github.com/oeasenet" {
		t.Errorf("org paths: %s %s", org.RunnersPath(), org.URL("https://github.com"))
	}
	repo, err := ParseTarget("oeasenet/platform")
	if err != nil || repo.Owner != "oeasenet" || repo.Repo != "platform" {
		t.Errorf("repo: %+v %v", repo, err)
	}
	if repo.RunnersPath() != "/repos/oeasenet/platform/actions/runners" || repo.URL("https://github.com/") != "https://github.com/oeasenet/platform" {
		t.Errorf("repo paths: %s %s", repo.RunnersPath(), repo.URL("https://github.com/"))
	}
	for _, bad := range []string{"", "a/b/c", "bad name", "/x", "x/"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

type ghRecorder struct {
	*httptest.Server
	paths   []string
	methods []string
	auth    string
	apiVer  string
}

func newFakeGitHubAPI(t *testing.T, handler http.HandlerFunc) *ghRecorder {
	t.Helper()
	rec := &ghRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.paths = append(rec.paths, r.URL.Path+"?"+r.URL.RawQuery)
		rec.methods = append(rec.methods, r.Method)
		rec.auth = r.Header.Get("Authorization")
		rec.apiVer = r.Header.Get("X-GitHub-Api-Version")
		handler(w, r)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func TestRegistrationToken_OrgAndRepo(t *testing.T) {
	gh := newFakeGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/registration-token") {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"AREGTOKEN","expires_at":"2026-08-24T12:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	})
	org := NewGitHubClient(gh.URL, "ghp_pat", Target{Owner: "oeasenet"})
	tok, err := org.RegistrationToken(context.Background())
	if err != nil || tok != "AREGTOKEN" {
		t.Fatalf("org token: %q %v", tok, err)
	}
	if gh.paths[0] != "/orgs/oeasenet/actions/runners/registration-token?" {
		t.Errorf("org path: %s", gh.paths[0])
	}
	if gh.auth != "Bearer ghp_pat" || gh.apiVer == "" {
		t.Errorf("headers: auth=%q apiver=%q", gh.auth, gh.apiVer)
	}

	repo := NewGitHubClient(gh.URL+"/", "ghp_pat", Target{Owner: "oeasenet", Repo: "platform"})
	if _, err := repo.RegistrationToken(context.Background()); err != nil {
		t.Fatalf("repo token: %v", err)
	}
	if gh.paths[1] != "/repos/oeasenet/platform/actions/runners/registration-token?" {
		t.Errorf("repo path: %s", gh.paths[1])
	}
}

func TestRegistrationToken_SurfacesGitHubErrorsWithoutPAT(t *testing.T) {
	gh := newFakeGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	})
	_, err := NewGitHubClient(gh.URL, "ghp_secret", Target{Owner: "oeasenet"}).RegistrationToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error should carry status and message, got: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "ghp_secret") {
		t.Error("error leaked the PAT")
	}
}

func TestListRunners_Paginates(t *testing.T) {
	gh := newFakeGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/oeasenet/actions/runners" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			_, _ = io.WriteString(w, `{"total_count":3,"runners":[
			  {"id":1,"name":"oease-aaaa1111","os":"Linux","status":"online","busy":true,"labels":[{"name":"self-hosted"},{"name":"docker"}]},
			  {"id":2,"name":"oease-bbbb2222","os":"Linux","status":"offline","busy":false,"labels":[]}]}`)
		case "2":
			_, _ = io.WriteString(w, `{"total_count":3,"runners":[{"id":3,"name":"other","os":"Linux","status":"online","busy":false,"labels":[]}]}`)
		default:
			_, _ = io.WriteString(w, `{"total_count":3,"runners":[]}`)
		}
	})
	runners, err := NewGitHubClient(gh.URL, "ghp_pat", Target{Owner: "oeasenet"}).ListRunners(context.Background())
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	if len(runners) != 3 {
		t.Fatalf("got %d runners, want 3 across pages: %+v", len(runners), runners)
	}
	if runners[0].ID != 1 || runners[0].Name != "oease-aaaa1111" || runners[0].Status != "online" || !runners[0].Busy {
		t.Errorf("first runner: %+v", runners[0])
	}
	if len(runners[0].Labels) != 2 || runners[0].Labels[1] != "docker" {
		t.Errorf("labels: %v", runners[0].Labels)
	}
	if !strings.Contains(gh.paths[0], "per_page=100") {
		t.Errorf("should request 100 per page: %s", gh.paths[0])
	}
}

func TestDeleteRunner_TreatsMissingAsDeleted(t *testing.T) {
	gh := newFakeGitHubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/oeasenet/actions/runners/1":
			w.WriteHeader(http.StatusNoContent)
		case "/orgs/oeasenet/actions/runners/2":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message":"Resource not accessible by personal access token"}`, http.StatusForbidden)
		}
	})
	c := NewGitHubClient(gh.URL, "ghp_pat", Target{Owner: "oeasenet"})
	if err := c.DeleteRunner(context.Background(), 1); err != nil {
		t.Errorf("204: %v", err)
	}
	if gh.methods[0] != http.MethodDelete {
		t.Errorf("method: %s", gh.methods[0])
	}
	if err := c.DeleteRunner(context.Background(), 2); err != nil {
		t.Errorf("404 means already gone: %v", err)
	}
	if err := c.DeleteRunner(context.Background(), 3); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("403 must be an error: %v", err)
	}
}
