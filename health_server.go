package frame

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

// healthStatus string values used in JSON responses.
const (
	statusOK      = "ok"
	statusDegraded = "degraded"
)

// HealthServerOption configures a HealthServer.
type HealthServerOption func(*healthServerConfig)

type healthServerConfig struct {
	addr         string
	readTimeout  time.Duration
	writeTimeout time.Duration
	logger       Logger
}

// WithHealthAddr sets the TCP address the server listens on.
// Defaults to ":8080".
func WithHealthAddr(addr string) HealthServerOption {
	return func(c *healthServerConfig) { c.addr = addr }
}

// WithHealthReadTimeout overrides the HTTP read timeout.
func WithHealthReadTimeout(d time.Duration) HealthServerOption {
	return func(c *healthServerConfig) { c.readTimeout = d }
}

// WithHealthWriteTimeout overrides the HTTP write timeout.
func WithHealthWriteTimeout(d time.Duration) HealthServerOption {
	return func(c *healthServerConfig) { c.writeTimeout = d }
}

// WithHealthLogger sets the logger used by the health server.
func WithHealthLogger(l Logger) HealthServerOption {
	return func(c *healthServerConfig) { c.logger = l }
}

// HealthServer is a Component that exposes three HTTP endpoints for
// orchestrators and load balancers:
//
//	GET /livez   — liveness probe: 200 while the process is alive.
//	GET /readyz  — readiness probe: 200 if all Critical/Significant components
//	              are healthy, 503 otherwise. Body contains per-component detail.
//	GET /healthz — alias for /readyz (Docker HEALTHCHECK compatibility).
//
// Register HealthServer as the first component in the Supervisor so that it
// starts before everything else (immediately serving /livez 200 and
// /readyz 503 while dependencies initialise) and stops last during shutdown
// (keeping the orchestrator informed until the very end).
//
// Example:
//
//	hs := frame.NewHealthServer(sup)
//	sup.Add(hs, frame.WithTier(frame.TierCritical))  // register first
//	sup.Add(myDB)
//	sup.Add(myCache, frame.WithTier(frame.TierSignificant))
type HealthServer struct {
	supervisor *Supervisor
	addr       string
	server     *http.Server
	logger     Logger

	// ready tracks whether Start has completed without error.
	mu    sync.RWMutex
	alive bool
}

// NewHealthServer creates a HealthServer backed by the given Supervisor for
// component health aggregation.
func NewHealthServer(sup *Supervisor, opts ...HealthServerOption) *HealthServer {
	cfg := healthServerConfig{
		addr:         defaultHealthAddr,
		readTimeout:  defaultHealthReadTimeout,
		writeTimeout: defaultHealthWriteTimeout,
		logger:       newNopLogger(),
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}

	hs := &HealthServer{
		supervisor: sup,
		addr:       cfg.addr,
		logger:     cfg.logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", hs.handleLivez)
	mux.HandleFunc("/readyz", hs.handleReadyz)
	mux.HandleFunc("/healthz", hs.handleReadyz) // alias

	hs.server = &http.Server{
		Addr:         cfg.addr,
		Handler:      mux,
		ReadTimeout:  cfg.readTimeout,
		WriteTimeout: cfg.writeTimeout,
	}
	return hs
}

func (h *HealthServer) Name() string { return "health-server" }

// Start begins listening and serving. It blocks until the server is shut down.
func (h *HealthServer) Start(_ context.Context) error {
	h.logger.Info("health server starting", "addr", h.addr)

	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return err
	}

	h.mu.Lock()
	h.alive = true
	h.mu.Unlock()

	h.logger.Info("health server listening", "addr", h.addr)

	if err := h.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop gracefully shuts down the HTTP server within the context deadline.
func (h *HealthServer) Stop(ctx context.Context) error {
	h.mu.Lock()
	h.alive = false
	h.mu.Unlock()

	h.logger.Info("health server stopping")
	return h.server.Shutdown(ctx)
}

// handleLivez responds 200 OK as long as the process is alive.
func (h *HealthServer) handleLivez(w http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	alive := h.alive
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if alive {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"status":"degraded"}`))
}

// componentHealthDetail is the per-component entry in a /readyz response.
type componentHealthDetail struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// readyzResponse is the full /readyz JSON body.
type readyzResponse struct {
	Status     string                  `json:"status"`
	Components []componentHealthDetail `json:"components"`
}

// handleReadyz aggregates the health of all registered components and responds
// 200 if all Critical and Significant components are healthy, 503 otherwise.
func (h *HealthServer) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if h.supervisor == nil {
		// No supervisor — just mirror liveness.
		h.handleLivez(w, nil)
		return
	}

	allHealthy := true
	details := make([]componentHealthDetail, 0, len(h.supervisor.statuses))

	for name, hs := range h.supervisor.statuses {
		err, known := hs.get()
		detail := componentHealthDetail{Name: name, Status: statusOK}

		if known && err != nil {
			// A known, non-nil error means the component is actively unhealthy.
			detail.Status = statusDegraded
			detail.Error = err.Error()

			// Only Critical and Significant components affect the overall status.
			mc := h.supervisor.components[name]
			if mc != nil && mc.tier != TierAuxiliary {
				allHealthy = false
			}
		}
		// !known means the component hasn't been health-checked yet (e.g. it
		// has no HealthChecker and manage hasn't run yet, or the supervisor
		// hasn't started). Treat as healthy — we don't penalise components for
		// being fast to start.

		details = append(details, detail)
	}

	resp := readyzResponse{Components: details}
	if allHealthy {
		resp.Status = statusOK
	} else {
		resp.Status = statusDegraded
	}

	code := http.StatusOK
	if !allHealthy {
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
