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
)

//go:embed templates/index.html
var templateFS embed.FS

// Server exposes the dashboard and the JSON API.
type Server struct {
	rec      *Reconciler
	password string
	tmpl     *template.Template
}

// dashboardData is what the template receives: the state, plus the same
// state as JSON for the client-side renderer.
type dashboardData struct {
	State
	StateJSON template.JS
}

func NewServer(rec *Reconciler, adminPassword string) (*Server, error) {
	tmpl, err := template.New("index.html").ParseFS(templateFS, "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}
	return &Server{rec: rec, password: adminPassword, tmpl: tmpl}, nil
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.Handle("GET /{$}", s.auth(s.handleDashboard))
	mux.Handle("GET /api/state", s.auth(s.handleState))
	mux.Handle("PUT /api/settings", s.auth(s.handleSettings))
	mux.Handle("PUT /api/pools/{name}", s.auth(s.handlePutPool))
	mux.Handle("DELETE /api/pools/{name}", s.auth(s.handleDeletePool))
	mux.Handle("POST /api/pools/{name}/scale", s.auth(s.handleScale))
	mux.Handle("POST /api/pools/{name}/pull", s.auth(s.handlePoolPull))
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
				w.Header().Set("WWW-Authenticate", `Basic realm="corral", charset="UTF-8"`)
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
	st := s.snapshot(r.Context())
	raw, err := json.Marshal(st)
	if err != nil {
		http.Error(w, "failed to render dashboard", http.StatusInternalServerError)
		return
	}
	// "</script>" inside strings would end the script element early.
	safe := strings.ReplaceAll(string(raw), "</", `<\/`)
	var buf bytes.Buffer
	if err := s.tmpl.Execute(&buf, dashboardData{State: st, StateJSON: template.JS(safe)}); err != nil {
		log.Printf("render dashboard: %v", err)
		http.Error(w, "failed to render dashboard", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// handleSettings changes the global switches (currently only auto_update).
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		AutoUpdate *bool `json:"auto_update"`
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	applied, err := s.rec.settings.Update(func(cur *Settings) error {
		if req.AutoUpdate != nil {
			cur.AutoUpdate = *req.AutoUpdate
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.rec.events.Infof("automatic updates %s", map[bool]string{true: "on", false: "off"}[applied.AutoUpdate])
	s.rec.Wake()
	writeJSON(w, http.StatusOK, map[string]bool{"auto_update": applied.AutoUpdate})
}

// handlePutPool creates a pool (from defaults) or updates an existing one
// (fields absent from the body keep their value).
func (s *Server) handlePutPool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var probe struct {
		Name *string `json:"name"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Name != nil && *probe.Name != name {
		writeError(w, http.StatusBadRequest, errors.New("a pool cannot be renamed; delete it and create a new one"))
		return
	}
	applied, err := s.rec.settings.Update(func(cur *Settings) error {
		p, ok := cur.Pool(name)
		if !ok {
			p = Pool{Name: name, Runtime: "ubuntu", DockerSocket: true, GracefulStopSeconds: defaultGracefulStopSeconds}
		}
		if err := decodeStrict(body, &p); err != nil {
			return err
		}
		p.Name = name
		cur.SetPool(p)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, _ := applied.Pool(name)
	s.rec.events.Infof("pool %s: runtime=%s image=%s count=%d labels=%q ephemeral=%v", p.Name, p.Runtime, p.EffectiveImage(), p.Count, p.Labels, p.Ephemeral)
	s.rec.Wake()
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.rec.settings.Get().Pool(name); !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no pool named %q", name))
		return
	}
	if _, err := s.rec.settings.Update(func(cur *Settings) error { cur.RemovePool(name); return nil }); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.rec.events.Infof("pool %s removed; its runners retire as they go idle", name)
	s.rec.Wake()
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "pool": name})
}

func (s *Server) handleScale(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Delta int  `json:"delta"`
		Count *int `json:"count"`
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, ok := s.rec.settings.Get().Pool(name); !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no pool named %q", name))
		return
	}
	applied, err := s.rec.settings.Update(func(cur *Settings) error {
		p, ok := cur.Pool(name)
		if !ok {
			return fmt.Errorf("no pool named %q", name)
		}
		if req.Count != nil {
			p.Count = *req.Count
		} else {
			p.Count += req.Delta
		}
		p.Count = max(0, min(maxRunnerCount, p.Count))
		cur.SetPool(p)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, _ := applied.Pool(name)
	s.rec.events.Infof("pool %s: desired runner count set to %d", name, p.Count)
	s.rec.Wake()
	writeJSON(w, http.StatusOK, map[string]int{"count": p.Count})
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

func (s *Server) startPull(w http.ResponseWriter, pool string) {
	switch err := s.rec.StartPullAndRoll(pool); {
	case errors.Is(err, errPullInProgress):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		writeError(w, http.StatusNotFound, err)
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pulling", "pool": pool})
	}
}

func (s *Server) handlePull(w http.ResponseWriter, _ *http.Request) { s.startPull(w, "") }
func (s *Server) handlePoolPull(w http.ResponseWriter, r *http.Request) {
	s.startPull(w, r.PathValue("name"))
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
