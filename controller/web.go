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
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed templates/index.html
var templateFS embed.FS

// Server exposes the dashboard and the JSON API.
type Server struct {
	rec      *Reconciler
	password string
	tmpl     *template.Template
}

func NewServer(rec *Reconciler, adminPassword string) (*Server, error) {
	tmpl, err := template.New("index.html").Funcs(template.FuncMap{
		"since": humanSince,
		"short": func(s string) string {
			if len(s) > 12 {
				return s[:12]
			}
			return s
		},
	}).ParseFS(templateFS, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}
	return &Server{rec: rec, password: adminPassword, tmpl: tmpl}, nil
}

func humanSince(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.Handle("GET /{$}", s.auth(s.handleDashboard))
	mux.Handle("GET /api/state", s.auth(s.handleState))
	mux.Handle("PUT /api/settings", s.auth(s.handleSettings))
	mux.Handle("POST /api/scale", s.auth(s.handleScale))
	mux.Handle("POST /api/runners/{name}/destroy", s.auth(s.handleDestroy))
	mux.Handle("POST /api/runners/{name}/recreate", s.auth(s.handleRecreate))
	mux.Handle("POST /api/pull", s.auth(s.handlePull))
	mux.Handle("POST /api/reconcile", s.auth(s.handleReconcile))
	return mux
}

// auth enforces HTTP basic auth when a password is configured.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.password != "" {
			_, pass, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="oease-gha-controller", charset="UTF-8"`)
				http.Error(w, "authentication required (user: admin, password: ADMIN_PASSWORD)", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version, "pulling": s.rec.pulling.Load()})
}

func (s *Server) snapshot(ctx context.Context) State {
	st, err := s.rec.Snapshot(ctx)
	if err != nil {
		st.DockerError = err.Error()
	}
	return st
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot(r.Context()))
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := s.tmpl.Execute(&buf, s.snapshot(r.Context())); err != nil {
		log.Printf("render dashboard: %v", err)
		http.Error(w, "failed to render dashboard", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// handleSettings merges the JSON body into the current settings, validates
// and persists them, then triggers a reconcile.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	applied, err := s.rec.settings.Update(func(cur *Settings) error {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		return dec.Decode(cur)
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.rec.events.Infof("settings updated: count=%d labels=%q image=%s ephemeral=%v", applied.Count, applied.Labels, applied.Image, applied.Ephemeral)
	s.rec.Wake()
	writeJSON(w, http.StatusOK, applied)
}

func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Delta int  `json:"delta"`
		Count *int `json:"count"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	applied, err := s.rec.settings.Update(func(cur *Settings) error {
		if req.Count != nil {
			cur.Count = *req.Count
		} else {
			cur.Count += req.Delta
		}
		if cur.Count < 0 {
			cur.Count = 0
		}
		if cur.Count > maxRunnerCount {
			cur.Count = maxRunnerCount
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.rec.events.Infof("desired runner count set to %d", applied.Count)
	s.rec.Wake()
	writeJSON(w, http.StatusOK, map[string]int{"count": applied.Count})
}

func (s *Server) runnerAction(w http.ResponseWriter, r *http.Request, action func(context.Context, string) error, verb string) {
	name := r.PathValue("name")
	if err := action(r.Context(), name); err != nil {
		if strings.Contains(err.Error(), "no runner named") {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.rec.Wake()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": verb, "runner": name})
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	s.runnerAction(w, r, s.rec.Destroy, "destroying")
}

func (s *Server) handleRecreate(w http.ResponseWriter, r *http.Request) {
	s.runnerAction(w, r, s.rec.Recreate, "recreating")
}

func (s *Server) handlePull(w http.ResponseWriter, _ *http.Request) {
	if s.rec.pulling.Load() {
		writeError(w, http.StatusConflict, errors.New("a pull is already in progress"))
		return
	}
	go func() {
		_ = s.rec.PullAndRoll(context.Background())
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pulling", "image": s.rec.settings.Get().Image})
}

func (s *Server) handleReconcile(w http.ResponseWriter, _ *http.Request) {
	s.rec.Wake()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "reconciling"})
}

func readBody(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, 64<<10)); err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if buf.Len() == 0 {
		return nil, errors.New("request body is required")
	}
	return buf.Bytes(), nil
}
