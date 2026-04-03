package frame

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	statusOK       = "ok"
	statusDegraded = "degraded"
)

// HealthReporter is the interface the HealthServer uses to query component
// health. *Supervisor satisfies this via HealthReportOrdered().
type HealthReporter interface {
	HealthReportOrdered() []NamedComponentStatus
}

// HealthServerOption configures a HealthServer.
type HealthServerOption func(*healthServerConfig)

type healthServerConfig struct {
	name         string
	addr         string
	readTimeout  time.Duration
	writeTimeout time.Duration
	logger       Logger
}

// WithHealthName overrides the component name returned by HealthServer.Name.
// This is useful when registering multiple HealthServer instances (e.g. on
// different ports) with the same Supervisor.
// Defaults to "health-server".
func WithHealthName(name string) HealthServerOption {
	return func(c *healthServerConfig) { c.name = name }
}

func WithHealthAddr(addr string) HealthServerOption {
	return func(c *healthServerConfig) { c.addr = addr }
}

func WithHealthReadTimeout(d time.Duration) HealthServerOption {
	return func(c *healthServerConfig) { c.readTimeout = d }
}

func WithHealthWriteTimeout(d time.Duration) HealthServerOption {
	return func(c *healthServerConfig) { c.writeTimeout = d }
}

func WithHealthLogger(l Logger) HealthServerOption {
	return func(c *healthServerConfig) { c.logger = l }
}

// HealthServer is a Component that exposes three HTTP endpoints:
//
//	GET /livez   — liveness:  200 while the process is alive
//	GET /readyz  — readiness: 200 if all Critical/Significant components healthy
//	GET /healthz — alias for /readyz (Docker HEALTHCHECK compatibility)
//
// Register HealthServer first with the Supervisor so it starts before
// everything else and stops last.
type HealthServer struct {
	name     string
	reporter HealthReporter
	addr     string
	server   *http.Server
	logger   Logger

	mu    sync.RWMutex
	alive bool
}

// NewHealthServer creates a HealthServer. Pass a *Supervisor as the reporter.
func NewHealthServer(reporter HealthReporter, opts ...HealthServerOption) *HealthServer {
	cfg := healthServerConfig{
		name:         "health-server",
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
		name:     cfg.name,
		reporter: reporter,
		addr:     cfg.addr,
		logger:   cfg.logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", hs.handleLivez)
	mux.HandleFunc("/readyz", hs.handleReadyz)
	mux.HandleFunc("/healthz", hs.handleReadyz)
	hs.server = &http.Server{
		Addr:         cfg.addr,
		Handler:      mux,
		ReadTimeout:  cfg.readTimeout,
		WriteTimeout: cfg.writeTimeout,
	}
	return hs
}

func (h *HealthServer) Name() string { return h.name }

// Start implements Component. It binds the TCP port, calls ready() to signal
// the supervisor, then serves until Stop is called.
func (h *HealthServer) Start(_ context.Context, ready func()) error {
	h.logger.Info("health server starting", "addr", h.addr)

	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return err
	}

	h.mu.Lock()
	h.alive = true
	h.mu.Unlock()

	h.logger.Info("health server listening", "addr", h.addr)
	ready() // TCP port is bound — supervisor proceeds to next component

	if err := h.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (h *HealthServer) Stop(ctx context.Context) error {
	h.mu.Lock()
	h.alive = false
	h.mu.Unlock()
	h.logger.Info("health server stopping")
	return h.server.Shutdown(ctx)
}

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

type componentHealthDetail struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	RestartCount int    `json:"restart_count,omitempty"`
}

type readyzResponse struct {
	Status     string                  `json:"status"`
	Components []componentHealthDetail `json:"components"`
}

func (h *HealthServer) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if h.reporter == nil {
		h.handleLivez(w, nil)
		return
	}
	report := h.reporter.HealthReportOrdered()
	allHealthy := true
	details := make([]componentHealthDetail, 0, len(report))
	for _, status := range report {
		detail := componentHealthDetail{Name: status.Name, Status: statusOK, RestartCount: status.RestartCount}
		if status.Known && status.Err != nil {
			detail.Status = statusDegraded
			detail.Error = status.Err.Error()
			if status.Tier != TierAuxiliary {
				allHealthy = false
			}
		}
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
