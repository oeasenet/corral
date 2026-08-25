package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DockerClient is a minimal Docker Engine API client (only what the
// controller needs), speaking to the daemon over a unix socket or TCP.
type DockerClient struct {
	http *http.Client
	base string
}

// NewDockerClient accepts DOCKER_HOST-style values: unix:///path,
// tcp://host:port, http(s)://host:port.
func NewDockerClient(host, apiVersion string) (*DockerClient, error) {
	if apiVersion == "" {
		apiVersion = "1.44"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("invalid docker host %q: %w", host, err)
	}
	transport := &http.Transport{}
	var base string
	switch u.Scheme {
	case "unix":
		sock := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		base = "http://docker"
	case "tcp", "http":
		base = "http://" + u.Host
	case "https":
		base = "https://" + u.Host
	default:
		return nil, fmt.Errorf("unsupported docker host %q (use unix:// or tcp://)", host)
	}
	// No client-wide timeout: stops and pulls legitimately take minutes.
	// Callers bound every call with a context.
	return &DockerClient{http: &http.Client{Transport: transport}, base: base + "/v" + apiVersion}, nil
}

// Container is the subset of container information the controller uses.
type Container struct {
	ID           string
	Name         string
	State        string // created, running, restarting, exited, dead, paused
	Status       string // human readable, e.g. "Up 5 minutes (healthy)"
	Health       string // healthy, unhealthy, starting or ""
	Image        string
	ImageID      string
	Labels       map[string]string
	Created      time.Time
	RestartCount int
	ExitCode     int
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name          string
	Image         string
	Hostname      string
	Env           []string
	Labels        map[string]string
	Binds         []string
	RestartPolicy string // "no", "unless-stopped", ...
	StopTimeout   int    // seconds Docker waits after SIGTERM before SIGKILL
}

// RegistryAuth is sent as X-Registry-Auth when pulling from a private registry.
type RegistryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// DockerError carries the daemon's status code and message.
type DockerError struct {
	Status  int
	Message string
}

func (e *DockerError) Error() string {
	return fmt.Sprintf("docker api: %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

func isDockerStatus(err error, status int) bool {
	var de *DockerError
	return errors.As(err, &de) && de.Status == status
}

func (c *DockerClient) do(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string) (*http.Response, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(data)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, payload)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker api %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(raw))
		}
		return nil, &DockerError{Status: resp.StatusCode, Message: msg.Message}
	}
	return resp, nil
}

func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// Ping checks that the daemon is reachable.
func (c *DockerClient) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/_ping", nil, nil, nil)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// ListContainers returns all containers (running or not) carrying the label filter, e.g. "key=value".
func (c *DockerClient) ListContainers(ctx context.Context, labelFilter string) ([]Container, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {labelFilter}})
	q := url.Values{"all": {"1"}, "filters": {string(filters)}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/json", q, nil, nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID      string `json:"Id"`
		Names   []string
		State   string
		Status  string
		Image   string
		ImageID string
		Created int64
		Labels  map[string]string
	}
	if err := decodeJSON(resp, &raw); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		name := ""
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		out = append(out, Container{
			ID: r.ID, Name: name, State: r.State, Status: r.Status, Health: healthFromStatus(r.Status),
			Image: r.Image, ImageID: r.ImageID, Labels: r.Labels, Created: time.Unix(r.Created, 0),
		})
	}
	return out, nil
}

// InspectContainer returns details for one container.
func (c *DockerClient) InspectContainer(ctx context.Context, id string) (Container, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, nil)
	if err != nil {
		return Container{}, err
	}
	var raw struct {
		ID           string `json:"Id"`
		Name         string
		Image        string
		RestartCount int
		Created      string
		State        struct {
			Status   string
			ExitCode int
			Health   *struct{ Status string }
		}
		Config struct {
			Image  string
			Labels map[string]string
		}
	}
	if err := decodeJSON(resp, &raw); err != nil {
		return Container{}, fmt.Errorf("decode container inspect: %w", err)
	}
	created, _ := time.Parse(time.RFC3339Nano, raw.Created)
	out := Container{
		ID: raw.ID, Name: strings.TrimPrefix(raw.Name, "/"), State: raw.State.Status, ExitCode: raw.State.ExitCode,
		Image: raw.Config.Image, ImageID: raw.Image, Labels: raw.Config.Labels, Created: created, RestartCount: raw.RestartCount,
	}
	if raw.State.Health != nil {
		out.Health = raw.State.Health.Status
	}
	return out, nil
}

// CreateContainer creates (but does not start) a container and returns its id.
func (c *DockerClient) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	type restartPolicy struct {
		Name string
	}
	body := struct {
		Image       string
		Hostname    string            `json:",omitempty"`
		Env         []string          `json:",omitempty"`
		Labels      map[string]string `json:",omitempty"`
		StopTimeout *int              `json:",omitempty"`
		HostConfig  struct {
			Binds         []string `json:",omitempty"`
			RestartPolicy restartPolicy
		}
	}{Image: spec.Image, Hostname: spec.Hostname, Env: spec.Env, Labels: spec.Labels}
	if spec.StopTimeout > 0 {
		t := spec.StopTimeout
		body.StopTimeout = &t
	}
	body.HostConfig.Binds = spec.Binds
	body.HostConfig.RestartPolicy = restartPolicy{Name: spec.RestartPolicy}
	if body.HostConfig.RestartPolicy.Name == "" {
		body.HostConfig.RestartPolicy.Name = "no"
	}

	resp, err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {spec.Name}}, body, nil)
	if err != nil {
		return "", err
	}
	var created struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	if err := decodeJSON(resp, &created); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	return created.ID, nil
}

// StartContainer starts a created container.
func (c *DockerClient) StartContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
	if err != nil {
		if isDockerStatus(err, http.StatusNotModified) {
			return nil // already running
		}
		return err
	}
	drain(resp)
	return nil
}

// StopContainer sends SIGTERM and waits up to timeout before Docker kills the container.
func (c *DockerClient) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/stop", q, nil, nil)
	if err != nil {
		if isDockerStatus(err, http.StatusNotModified) || isDockerStatus(err, http.StatusNotFound) {
			return nil // already stopped / gone
		}
		return err
	}
	drain(resp)
	return nil
}

// RemoveContainer deletes a container (and its anonymous volumes).
func (c *DockerClient) RemoveContainer(ctx context.Context, id string, force bool) error {
	q := url.Values{"v": {"1"}}
	if force {
		q.Set("force", "1")
	}
	resp, err := c.do(ctx, http.MethodDelete, "/containers/"+id, q, nil, nil)
	if err != nil {
		if isDockerStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	drain(resp)
	return nil
}

// splitImageRef separates "repo:tag" into repository and tag; digests are
// returned whole (Docker resolves them via fromImage alone).
func splitImageRef(ref string) (repo, tag string) {
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}

// PullImage pulls an image, optionally authenticating against its registry.
func (c *DockerClient) PullImage(ctx context.Context, ref string, auth *RegistryAuth) error {
	repo, tag := splitImageRef(ref)
	q := url.Values{"fromImage": {repo}}
	if tag != "" {
		q.Set("tag", tag)
	}
	headers := map[string]string{}
	if auth != nil && auth.Username != "" {
		raw, _ := json.Marshal(auth)
		headers["X-Registry-Auth"] = base64.URLEncoding.EncodeToString(raw)
	}
	resp, err := c.do(ctx, http.MethodPost, "/images/create", q, nil, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The response is a stream of JSON progress messages; errors arrive in-band.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("pull %s: read progress stream: %w", ref, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, msg.Error)
		}
	}
}

// ImageInfo is the subset of image details the controller uses.
type ImageInfo struct {
	ID     string
	Labels map[string]string
}

// InspectImage returns the local image's id and labels; a zero ImageInfo
// (no error) when the image is not present.
func (c *DockerClient) InspectImage(ctx context.Context, ref string) (ImageInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil)
	if err != nil {
		if isDockerStatus(err, http.StatusNotFound) {
			return ImageInfo{}, nil
		}
		return ImageInfo{}, err
	}
	var img struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string
		}
	}
	if err := decodeJSON(resp, &img); err != nil {
		return ImageInfo{}, fmt.Errorf("decode image inspect: %w", err)
	}
	return ImageInfo{ID: img.ID, Labels: img.Config.Labels}, nil
}
