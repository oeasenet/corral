package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const githubAPIVersion = "2022-11-28"

var (
	validOwner = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	validRepo  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Target is where runners register: an organization or a single repository.
type Target struct {
	Owner string
	Repo  string // empty for organization-level runners
}

// ParseTarget accepts "org" or "owner/repo".
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 1:
		if !validOwner.MatchString(parts[0]) {
			return Target{}, fmt.Errorf("invalid organization name %q", s)
		}
		return Target{Owner: parts[0]}, nil
	case 2:
		if !validOwner.MatchString(parts[0]) || !validRepo.MatchString(parts[1]) {
			return Target{}, fmt.Errorf("invalid owner/repo %q", s)
		}
		return Target{Owner: parts[0], Repo: parts[1]}, nil
	default:
		return Target{}, fmt.Errorf("target must be an organization or owner/repo, got %q", s)
	}
}

func (t Target) String() string {
	if t.Repo == "" {
		return t.Owner
	}
	return t.Owner + "/" + t.Repo
}

// RunnersPath is the REST path prefix for the target's self-hosted runners.
func (t Target) RunnersPath() string {
	if t.Repo == "" {
		return "/orgs/" + t.Owner + "/actions/runners"
	}
	return "/repos/" + t.Owner + "/" + t.Repo + "/actions/runners"
}

// URL is the web URL runners register against.
func (t Target) URL(base string) string {
	return strings.TrimRight(base, "/") + "/" + t.String()
}

// Runner is a registered self-hosted runner as reported by GitHub.
type Runner struct {
	ID     int64
	Name   string
	Status string // online / offline
	Busy   bool
	Labels []string
}

// GitHubClient talks to the GitHub REST API on behalf of one target.
type GitHubClient struct {
	http   *http.Client
	api    string
	token  string
	target Target
}

func NewGitHubClient(api, token string, target Target) *GitHubClient {
	return &GitHubClient{
		http:   &http.Client{Timeout: 30 * time.Second},
		api:    strings.TrimRight(api, "/"),
		token:  token,
		target: target,
	}
}

func (g *GitHubClient) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, g.api+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "oease-gha-controller/"+version)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github api %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &body)
		if body.Message == "" {
			body.Message = strings.TrimSpace(string(raw))
		}
		return nil, &GitHubError{Status: resp.StatusCode, Message: body.Message, Path: path}
	}
	return resp, nil
}

// GitHubError carries GitHub's status code and message.
type GitHubError struct {
	Status  int
	Message string
	Path    string
}

func (e *GitHubError) Error() string {
	return fmt.Sprintf("github api returned %d for %s: %s", e.Status, e.Path, e.Message)
}

func isGitHubStatus(err error, status int) bool {
	var ge *GitHubError
	return errors.As(err, &ge) && ge.Status == status
}

// RegistrationToken mints a short-lived runner registration token.
func (g *GitHubClient) RegistrationToken(ctx context.Context) (string, error) {
	resp, err := g.do(ctx, http.MethodPost, g.target.RunnersPath()+"/registration-token")
	if err != nil {
		return "", err
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(resp, &body); err != nil || body.Token == "" {
		return "", errors.New("github api returned an unexpected registration-token response")
	}
	return body.Token, nil
}

// ListRunners returns every self-hosted runner registered on the target.
func (g *GitHubClient) ListRunners(ctx context.Context) ([]Runner, error) {
	const perPage = 100
	var out []Runner
	for page := 1; page <= 20; page++ {
		resp, err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=%d&page=%d", g.target.RunnersPath(), perPage, page))
		if err != nil {
			return nil, err
		}
		var body struct {
			TotalCount int `json:"total_count"`
			Runners    []struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Busy   bool   `json:"busy"`
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
			} `json:"runners"`
		}
		if err := decodeJSON(resp, &body); err != nil {
			return nil, fmt.Errorf("decode runner list: %w", err)
		}
		for _, r := range body.Runners {
			runner := Runner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy}
			for _, l := range r.Labels {
				runner.Labels = append(runner.Labels, l.Name)
			}
			out = append(out, runner)
		}
		if len(body.Runners) == 0 || len(out) >= body.TotalCount {
			break
		}
	}
	return out, nil
}

// DeleteRunner removes a runner registration. A missing runner is not an error.
func (g *GitHubClient) DeleteRunner(ctx context.Context, id int64) error {
	resp, err := g.do(ctx, http.MethodDelete, g.target.RunnersPath()+"/"+strconv.FormatInt(id, 10))
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return nil
		}
		return err
	}
	drain(resp)
	return nil
}
