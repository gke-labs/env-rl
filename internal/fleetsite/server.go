// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package fleetsite serves the sandbox fleet site: the Fleet and Cluster
// pages and the JSON API over the clusters' inventory snapshots, and the
// actor events from BigQuery (docs/plans/sandbox-fleet-site). It only reads:
// it holds no cluster credential and calls no write API.
package fleetsite

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/inventory"
	"golang.org/x/time/rate"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Per-user request rate: IAP admits the group's members, and the limit
// bounds what one of them makes the site query.
const (
	userRate  = rate.Limit(10)
	userBurst = 40
	// Each events request is one BigQuery job, so that route has a lower
	// limit of its own on top of the general one.
	eventsRate  = rate.Limit(1)
	eventsBurst = 5
	// limiterIdle is how long an unused per-user limiter is kept.
	limiterIdle = 10 * time.Minute
)

// maxEventsWindow bounds how far back since may reach.
const maxEventsWindow = 30 * 24 * time.Hour

// defaultEventsWindow is the window without since.
const defaultEventsWindow = 24 * time.Hour

// requestTimeout bounds the reads of one request.
const requestTimeout = 30 * time.Second

// actorUID matches a Substrate actor uid.
var actorUID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// EventSource returns one actor's lifecycle events.
type EventSource interface {
	ActorEvents(ctx context.Context, uid string, since time.Time) ([]Event, error)
}

// Authenticate returns the email of the request's user, or an error that
// denies the request.
type Authenticate func(r *http.Request) (string, error)

// IAPAuth authenticates a request by its IAP assertion.
func IAPAuth(v *IAPVerifier) Authenticate {
	return func(r *http.Request) (string, error) {
		return v.Verify(r.Context(), r.Header.Get(IAPHeader))
	}
}

// Config wires a Server.
type Config struct {
	Snapshots *Snapshots
	// Events is nil when no events table is configured; the events API
	// then answers 503.
	Events EventSource
	Auth   Authenticate
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Version is shown in the page footer.
	Version string
}

// Server is the site's HTTP handler.
type Server struct {
	cfg   Config
	pages map[string]*template.Template
	mux   *http.ServeMux

	mu       sync.Mutex
	limiters map[string]*userLimiter
	swept    time.Time
	css      []byte
}

type userLimiter struct {
	lim, events *rate.Limiter
	seen        time.Time
}

// New returns the site's handler.
func New(cfg Config) (*Server, error) {
	if cfg.Snapshots == nil || cfg.Auth == nil {
		return nil, errors.New("fleetsite: Snapshots and Auth are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{cfg: cfg, pages: map[string]*template.Template{}, limiters: map[string]*userLimiter{}}
	for _, page := range []string{"fleet.html", "cluster.html", "error.html"} {
		t, err := template.New(page).Funcs(templateFuncs).ParseFS(assets, "templates/layout.html", "templates/"+page)
		if err != nil {
			return nil, err
		}
		s.pages[page] = t
	}
	css, err := fs.ReadFile(assets, "static/site.css")
	if err != nil {
		return nil, err
	}
	s.css = css

	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", s.fleetPage)
	app.HandleFunc("GET /clusters/{project}/{cluster}", s.clusterPage)
	app.HandleFunc("GET /api/v1/clusters", s.apiClusters)
	app.HandleFunc("GET /api/v1/clusters/{project}/{cluster}", s.apiCluster)
	app.HandleFunc("GET /api/v1/actors/{uid}/events", s.apiActorEvents)
	app.HandleFunc("GET /static/site.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(s.css)
	})

	s.mux = http.NewServeMux()
	// The health check carries no data; Cloud Run probes it without IAP.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.Handle("/", s.authenticated(app))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Strict-Transport-Security", "max-age=31536000")
	h.Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mux.ServeHTTP(w, r)
}

type userKey struct{}

// authenticated denies a request without a valid identity, then applies the
// user's rate limit.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The deadline covers the assertion check too, which can fetch
		// IAP's keys.
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		email, err := s.cfg.Auth(r)
		if err != nil {
			log.Printf("fleetsite: denied %s %q: %v", r.Method, r.URL.Path, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !s.allow(email) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userKey{}, email)))
	})
}

func (s *Server) allow(email string) bool {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Sweep idle limiters at most once per limiterIdle, so a request does
	// not walk every user.
	if now.Sub(s.swept) > limiterIdle {
		for k, l := range s.limiters {
			if now.Sub(l.seen) > limiterIdle {
				delete(s.limiters, k)
			}
		}
		s.swept = now
	}
	l, ok := s.limiters[email]
	if !ok {
		l = &userLimiter{lim: rate.NewLimiter(userRate, userBurst), events: rate.NewLimiter(eventsRate, eventsBurst)}
		s.limiters[email] = l
	}
	l.seen = now
	return l.lim.AllowN(now, 1)
}

// allowEvents applies the events route's own limit to the user.
func (s *Server) allowEvents(email string) bool {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.limiters[email]
	if !ok {
		return false
	}
	return l.events.AllowN(now, 1)
}

func user(r *http.Request) string {
	email, _ := r.Context().Value(userKey{}).(string)
	return email
}

// force reports whether the request asks to revalidate the snapshots now.
func force(r *http.Request) bool { return r.URL.Query().Get("nocache") == "1" }

// pathCluster reads and checks the {project} and {cluster} path segments.
func pathCluster(r *http.Request) (string, string, bool) {
	project, cluster := r.PathValue("project"), r.PathValue("cluster")
	if project == "" || cluster == "" ||
		inventory.CheckPathSegment("project", project) != nil || inventory.CheckPathSegment("cluster", cluster) != nil {
		return "", "", false
	}
	return project, cluster, true
}

// --- JSON API -----------------------------------------------------------------

// ClusterSummary is one row of GET /api/v1/clusters.
type ClusterSummary struct {
	Project     string                 `json:"project"`
	Cluster     string                 `json:"cluster"`
	Location    string                 `json:"location,omitempty"`
	GeneratedAt *time.Time             `json:"generated_at,omitempty"`
	AgeSeconds  float64                `json:"age_s"`
	Stale       bool                   `json:"stale"`
	Reporter    *inventory.Reporter    `json:"reporter,omitempty"`
	Counts      *inventory.Counts      `json:"counts,omitempty"`
	Deployments []inventory.Deployment `json:"deployments,omitempty"`
	WorkerPools []inventory.WorkerPool `json:"workerpools,omitempty"`
	Error       string                 `json:"error,omitempty"`
}

func summarize(c Cluster, now time.Time) ClusterSummary {
	out := ClusterSummary{Project: c.Project, Cluster: c.Cluster, Stale: c.Stale(now), Error: c.Err}
	if snap := c.Snapshot; snap != nil {
		out.Location = snap.Location
		out.GeneratedAt = &snap.GeneratedAt
		out.AgeSeconds = c.Age(now).Seconds()
		out.Reporter = &snap.Reporter
		out.Counts = &snap.Counts
		out.Deployments = snap.Deployments
		out.WorkerPools = snap.WorkerPools
	}
	return out
}

func (s *Server) apiClusters(w http.ResponseWriter, r *http.Request) {
	clusters, err := s.cfg.Snapshots.Clusters(r.Context(), force(r))
	if err != nil {
		s.apiError(w, r, http.StatusBadGateway, "the inventory bucket could not be listed", err)
		return
	}
	now := s.cfg.Now()
	rows := make([]ClusterSummary, 0, len(clusters))
	for _, c := range clusters {
		rows = append(rows, summarize(c, now))
	}
	writeJSON(w, map[string]any{"clusters": rows})
}

// ClusterDetail is GET /api/v1/clusters/{project}/{cluster}: the snapshot
// as the reporter wrote it, with the site's view of its age.
type ClusterDetail struct {
	Project    string              `json:"project"`
	Cluster    string              `json:"cluster"`
	Generation int64               `json:"generation"`
	AgeSeconds float64             `json:"age_s"`
	Stale      bool                `json:"stale"`
	Error      string              `json:"error,omitempty"`
	Snapshot   *inventory.Snapshot `json:"snapshot,omitempty"`
}

func (s *Server) apiCluster(w http.ResponseWriter, r *http.Request) {
	project, cluster, ok := pathCluster(r)
	if !ok {
		s.apiError(w, r, http.StatusBadRequest, "project and cluster must be one path segment each", nil)
		return
	}
	c, found, err := s.cfg.Snapshots.Cluster(r.Context(), project, cluster, force(r))
	if err != nil {
		s.apiError(w, r, http.StatusBadGateway, "the inventory bucket could not be listed", err)
		return
	}
	if !found {
		s.apiError(w, r, http.StatusNotFound, "no snapshot for this cluster", nil)
		return
	}
	now := s.cfg.Now()
	writeJSON(w, ClusterDetail{
		Project: c.Project, Cluster: c.Cluster, Generation: c.Generation,
		AgeSeconds: c.Age(now).Seconds(), Stale: c.Stale(now), Error: c.Err, Snapshot: c.Snapshot,
	})
}

func (s *Server) apiActorEvents(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !actorUID.MatchString(uid) {
		s.apiError(w, r, http.StatusBadRequest, "uid is not an actor uid", nil)
		return
	}
	now := s.cfg.Now()
	since := now.Add(-defaultEventsWindow)
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.apiError(w, r, http.StatusBadRequest, "since is not an RFC 3339 time", nil)
			return
		}
		if t.After(now) || now.Sub(t) > maxEventsWindow {
			s.apiError(w, r, http.StatusBadRequest, "since must be in the last 30 days", nil)
			return
		}
		since = t
	}
	if s.cfg.Events == nil {
		s.apiError(w, r, http.StatusServiceUnavailable, "no events table is configured", nil)
		return
	}
	if !s.allowEvents(user(r)) {
		w.Header().Set("Retry-After", "1")
		s.apiError(w, r, http.StatusTooManyRequests, "too many events requests", nil)
		return
	}
	events, err := s.cfg.Events.ActorEvents(r.Context(), uid, since)
	if err != nil {
		s.apiError(w, r, http.StatusBadGateway, "the events could not be read", err)
		return
	}
	writeJSON(w, map[string]any{"uid": uid, "since": since.UTC(), "events": events, "truncated": len(events) >= MaxEvents})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Compact: a large cluster's snapshot is tens of megabytes.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("fleetsite: writing a response: %v", err)
	}
}

// apiError answers with a generic message; err, the detail, goes to the log.
func (s *Server) apiError(w http.ResponseWriter, r *http.Request, code int, msg string, err error) {
	if err != nil {
		log.Printf("fleetsite: %s %q: %s: %v", r.Method, r.URL.Path, msg, err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// pageError renders the error page with a generic message.
func (s *Server) pageError(w http.ResponseWriter, r *http.Request, code int, msg string, err error) {
	if err != nil {
		log.Printf("fleetsite: %s %q: %s: %v", r.Method, r.URL.Path, msg, err)
	}
	s.render(w, r, code, "error.html", map[string]any{"Message": msg})
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, code int, page string, data map[string]any) {
	data["User"] = user(r)
	data["Version"] = s.cfg.Version
	data["Now"] = s.cfg.Now().UTC()
	// Render first, so a template error is a 500 and not half a page.
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("fleetsite: rendering %s: %v", page, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}
