package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDocker is a minimal Docker Engine API stand-in that records requests.
type fakeDocker struct {
	*httptest.Server
	mu       chan struct{}
	requests []recordedRequest
	handler  func(w http.ResponseWriter, r *http.Request, body []byte) bool // return true if handled
}

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

func newFakeDocker(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte) bool) *fakeDocker {
	t.Helper()
	f := &fakeDocker{mu: make(chan struct{}, 1), handler: handler}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu <- struct{}{}
		f.requests = append(f.requests, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		<-f.mu
		if f.handler != nil && f.handler(w, r, body) {
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDocker) client(t *testing.T) *DockerClient {
	t.Helper()
	c, err := NewDockerClient("tcp://"+strings.TrimPrefix(f.URL, "http://"), "1.44")
	if err != nil {
		t.Fatalf("NewDockerClient: %v", err)
	}
	return c
}

func (f *fakeDocker) last() recordedRequest {
	f.mu <- struct{}{}
	defer func() { <-f.mu }()
	return f.requests[len(f.requests)-1]
}

func TestNewDockerClient_ResolvesHosts(t *testing.T) {
	cases := map[string]string{
		"unix:///var/run/docker.sock": "http://docker/v1.44",
		"tcp://10.0.0.5:2375":         "http://10.0.0.5:2375/v1.44",
		"http://10.0.0.5:2375":        "http://10.0.0.5:2375/v1.44",
	}
	for host, want := range cases {
		c, err := NewDockerClient(host, "1.44")
		if err != nil {
			t.Errorf("%s: unexpected error %v", host, err)
			continue
		}
		if c.base != want {
			t.Errorf("%s: base %q, want %q", host, c.base, want)
		}
	}
	if _, err := NewDockerClient("ftp://nope", "1.44"); err == nil {
		t.Error("unsupported scheme must be rejected")
	}
}

func TestListContainers_FiltersByLabelAndParsesFields(t *testing.T) {
	fd := newFakeDocker(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/v1.44/containers/json" {
			return false
		}
		_, _ = io.WriteString(w, `[
		  {"Id":"c1","Names":["/oease-aaaa1111"],"State":"running","Status":"Up 5 minutes (healthy)","Image":"img:latest","ImageID":"sha256:111","Created":1700000000,
		   "Labels":{"dev.oease.gha.managed":"true","dev.oease.gha.name":"oease-aaaa1111"}},
		  {"Id":"c2","Names":["/oease-bbbb2222"],"State":"exited","Status":"Exited (1) 2 minutes ago","Image":"img:latest","ImageID":"sha256:111","Created":1700000100,
		   "Labels":{"dev.oease.gha.managed":"true"}}
		]`)
		return true
	})
	list, err := fd.client(t).ListContainers(context.Background(), "dev.oease.gha.managed=true")
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	req := fd.last()
	if !strings.Contains(req.Query, "all=1") {
		t.Errorf("must list stopped containers too, query: %s", req.Query)
	}
	if !strings.Contains(req.Query, "dev.oease.gha.managed") {
		t.Errorf("label filter missing from query: %s", req.Query)
	}
	if len(list) != 2 {
		t.Fatalf("got %d containers, want 2", len(list))
	}
	c := list[0]
	if c.ID != "c1" || c.Name != "oease-aaaa1111" || c.State != "running" || c.ImageID != "sha256:111" {
		t.Errorf("parsed container: %+v", c)
	}
	if c.Health != "healthy" {
		t.Errorf("health from status text: got %q", c.Health)
	}
	if c.Labels["dev.oease.gha.name"] != "oease-aaaa1111" {
		t.Errorf("labels: %v", c.Labels)
	}
	if c.Created.Unix() != 1700000000 {
		t.Errorf("created: %v", c.Created)
	}
	if list[1].State != "exited" || list[1].Health != "" {
		t.Errorf("second container: %+v", list[1])
	}
}

func TestInspectContainer_ReadsHealthAndRestartCount(t *testing.T) {
	fd := newFakeDocker(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.URL.Path != "/v1.44/containers/c1/json" {
			return false
		}
		_, _ = io.WriteString(w, `{"Id":"c1","Name":"/oease-aaaa1111","Image":"sha256:111","RestartCount":3,"Created":"2026-08-24T10:00:00.000000000Z",
		  "State":{"Status":"running","Running":true,"ExitCode":0,"Health":{"Status":"unhealthy"}},
		  "Config":{"Image":"img:latest","Labels":{"dev.oease.gha.managed":"true"}}}`)
		return true
	})
	c, err := fd.client(t).InspectContainer(context.Background(), "c1")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if c.Name != "oease-aaaa1111" || c.State != "running" || c.Health != "unhealthy" || c.RestartCount != 3 || c.ImageID != "sha256:111" {
		t.Errorf("inspect: %+v", c)
	}
}

func TestCreateAndStartContainer_SendsExpectedSpec(t *testing.T) {
	fd := newFakeDocker(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/create":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"newid","Warnings":[]}`)
			return true
		case r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/newid/start":
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	})
	c := fd.client(t)
	spec := ContainerSpec{
		Name:          "oease-aaaa1111",
		Image:         "ghcr.io/oeasenet/gha-docker-runner/runner:latest",
		Hostname:      "oease-aaaa1111",
		Env:           []string{"RUNNER_TOKEN=abc", "RUNNER_NAME=oease-aaaa1111"},
		Labels:        map[string]string{"dev.oease.gha.managed": "true"},
		Binds:         []string{"/var/run/docker.sock:/var/run/docker.sock"},
		RestartPolicy: "unless-stopped",
		StopTimeout:   960,
	}
	id, err := c.CreateContainer(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if id != "newid" {
		t.Errorf("id: %q", id)
	}
	req := fd.last()
	if !strings.Contains(req.Query, "name=oease-aaaa1111") {
		t.Errorf("container name must be passed as query, got %s", req.Query)
	}
	var body struct {
		Image       string
		Hostname    string
		Env         []string
		Labels      map[string]string
		StopTimeout int
		HostConfig  struct {
			Binds         []string
			RestartPolicy struct{ Name string }
		}
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, req.Body)
	}
	if body.Image != spec.Image || body.Hostname != spec.Hostname || body.StopTimeout != 960 {
		t.Errorf("body: %+v", body)
	}
	if len(body.Env) != 2 || body.Env[0] != "RUNNER_TOKEN=abc" {
		t.Errorf("env: %v", body.Env)
	}
	if body.Labels["dev.oease.gha.managed"] != "true" {
		t.Errorf("labels: %v", body.Labels)
	}
	if len(body.HostConfig.Binds) != 1 || body.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("host config: %+v", body.HostConfig)
	}
	if err := c.StartContainer(context.Background(), id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if last := fd.last(); last.Path != "/v1.44/containers/newid/start" {
		t.Errorf("start path: %s", last.Path)
	}
}

func TestStopContainer_PassesTimeoutAndAcceptsAlreadyStopped(t *testing.T) {
	fd := newFakeDocker(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/c1/stop" {
			w.WriteHeader(http.StatusNotModified) // already stopped
			return true
		}
		return false
	})
	if err := fd.client(t).StopContainer(context.Background(), "c1", 90*time.Second); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if q := fd.last().Query; !strings.Contains(q, "t=90") {
		t.Errorf("timeout seconds missing from query: %s", q)
	}
}

func TestRemoveContainer_ForcesAndTreatsMissingAsRemoved(t *testing.T) {
	fd := newFakeDocker(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodDelete && r.URL.Path == "/v1.44/containers/gone" {
			http.Error(w, `{"message":"No such container: gone"}`, http.StatusNotFound)
			return true
		}
		return false
	})
	if err := fd.client(t).RemoveContainer(context.Background(), "gone", true); err != nil {
		t.Fatalf("a missing container is already removed, got: %v", err)
	}
	q := fd.last().Query
	if !strings.Contains(q, "force=1") || !strings.Contains(q, "v=1") {
		t.Errorf("remove must force and drop anonymous volumes, query: %s", q)
	}
}

func TestPullImage_SendsRegistryAuthAndDetectsStreamErrors(t *testing.T) {
	var fail bool
	fd := newFakeDocker(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/v1.44/images/create" {
			_, _ = io.WriteString(w, `{"status":"Pulling from oeasenet/gha-docker-runner/runner"}`+"\n")
			if fail {
				_, _ = io.WriteString(w, `{"error":"manifest unknown","errorDetail":{"message":"manifest unknown"}}`+"\n")
			} else {
				_, _ = io.WriteString(w, `{"status":"Status: Image is up to date"}`+"\n")
			}
			return true
		}
		return false
	})
	c := fd.client(t)
	auth := &RegistryAuth{Username: "tony", Password: "ghp_x"}
	if err := c.PullImage(context.Background(), "ghcr.io/oeasenet/gha-docker-runner/runner:latest", auth); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	req := fd.last()
	if !strings.Contains(req.Query, "fromImage=ghcr.io%2Foeasenet%2Fgha-docker-runner%2Frunner") || !strings.Contains(req.Query, "tag=latest") {
		t.Errorf("pull query: %s", req.Query)
	}
	raw, err := base64.URLEncoding.DecodeString(req.Header.Get("X-Registry-Auth"))
	if err != nil || !strings.Contains(string(raw), `"username":"tony"`) || !strings.Contains(string(raw), `"password":"ghp_x"`) {
		t.Errorf("X-Registry-Auth header: %q (%v)", raw, err)
	}

	fail = true
	if err := c.PullImage(context.Background(), "ghcr.io/oeasenet/gha-docker-runner/runner:latest", nil); err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("stream errors must surface, got: %v", err)
	}
	if fd.last().Header.Get("X-Registry-Auth") != "" {
		t.Error("no auth header expected without credentials")
	}
}

func TestImageID_EmptyWhenImageMissing(t *testing.T) {
	fd := newFakeDocker(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		switch r.URL.Path {
		case "/v1.44/images/img:present/json":
			_, _ = io.WriteString(w, `{"Id":"sha256:abc"}`)
			return true
		case "/v1.44/images/img:missing/json":
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
			return true
		}
		return false
	})
	c := fd.client(t)
	if id, err := c.ImageID(context.Background(), "img:present"); err != nil || id != "sha256:abc" {
		t.Errorf("present: %q %v", id, err)
	}
	if id, err := c.ImageID(context.Background(), "img:missing"); err != nil || id != "" {
		t.Errorf("missing: %q %v", id, err)
	}
}

func TestDockerErrors_IncludeDaemonMessage(t *testing.T) {
	fd := newFakeDocker(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		http.Error(w, `{"message":"conflict: name already in use"}`, http.StatusConflict)
		return true
	})
	_, err := fd.client(t).CreateContainer(context.Background(), ContainerSpec{Name: "x", Image: "y"})
	if err == nil || !strings.Contains(err.Error(), "name already in use") || !strings.Contains(err.Error(), "409") {
		t.Errorf("error should carry status and daemon message, got: %v", err)
	}
}
