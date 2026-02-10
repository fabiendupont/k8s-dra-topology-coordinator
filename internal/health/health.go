// Package health provides health check functionality for the Node Partition DRA Driver.
// This package implements HTTP health checks that work in distroless containers
// without requiring external tools like curl.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"

	klog "k8s.io/klog/v2"
)

const (
	statusHealthy   = "healthy"
	statusUnhealthy = "unhealthy"
	// DefaultHealthPort is the default port for health checks
	DefaultHealthPort = 8080
	// DefaultHealthPath is the default health check endpoint
	DefaultHealthPath = "/healthz"
	// DefaultReadTimeout is the default read timeout for health checks
	DefaultReadTimeout = 5 * time.Second
	// DefaultWriteTimeout is the default write timeout for health checks
	DefaultWriteTimeout = 10 * time.Second
)

// Status represents the health status of the application
type Status struct {
	Status    string           `json:"status"`
	Timestamp time.Time        `json:"timestamp"`
	Version   string           `json:"version"`
	Checks    map[string]Check `json:"checks,omitempty"`
}

// Check represents an individual health check
type Check struct {
	Status  string                 `json:"status"`
	Message string                 `json:"message,omitempty"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// Checker provides health check functionality
type Checker struct {
	server  *http.Server
	mux     *http.ServeMux
	status  *Status
	mu      sync.RWMutex
	port    int
	path    string
	version string
	checks  map[string]CheckFunc
	started bool
}

// CheckFunc is a function that performs a health check
type CheckFunc func(ctx context.Context) Check

// NewChecker creates a new health checker
func NewChecker(port int, version string) *Checker {
	if port == 0 {
		port = DefaultHealthPort
	}

	hc := &Checker{
		port:    port,
		path:    DefaultHealthPath,
		version: version,
		checks:  make(map[string]CheckFunc),
		status: &Status{
			Status:    "healthy",
			Timestamp: time.Now(),
			Version:   version,
			Checks:    make(map[string]Check),
		},
	}

	// Set up HTTP server
	hc.mux = http.NewServeMux()
	hc.mux.HandleFunc(hc.path, hc.handleHealthCheck)
	hc.mux.HandleFunc("/", hc.handleNotFound)

	hc.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      hc.mux,
		ReadTimeout:  DefaultReadTimeout,
		WriteTimeout: DefaultWriteTimeout,
	}

	return hc
}

// Handle registers an additional HTTP handler on the health server.
// Must be called before Start.
func (hc *Checker) Handle(pattern string, handler http.Handler) {
	hc.mux.Handle(pattern, handler)
}

// AddCheck adds a health check function
func (hc *Checker) AddCheck(name string, check CheckFunc) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.checks[name] = check
}

// Start starts the health check server
func (hc *Checker) Start(ctx context.Context) error {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if hc.started {
		return fmt.Errorf("health checker already started")
	}

	klog.Infof("Starting health check server on port %d", hc.port)

	// Start server in a goroutine
	go func() {
		if err := hc.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Errorf("Health check server error: %v", err)
		}
	}()

	// Start background health check updates
	go hc.updateStatus(ctx)

	// Mark as started only after goroutines are launched
	hc.started = true

	return nil
}

// Stop stops the health check server
func (hc *Checker) Stop(ctx context.Context) error {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if !hc.started {
		return nil
	}

	klog.Info("Stopping health check server")
	if err := hc.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown health check server: %w", err)
	}
	return nil
}

// updateStatus periodically updates the health status
func (hc *Checker) updateStatus(ctx context.Context) {
	const healthCheckInterval = 30 * time.Second
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.performHealthChecks(ctx)
		}
	}
}

// performHealthChecks runs all registered health checks
func (hc *Checker) performHealthChecks(ctx context.Context) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	overallStatus := statusHealthy
	checks := make(map[string]Check)

	// Run all health checks
	for name, checkFunc := range hc.checks {
		check := checkFunc(ctx)
		checks[name] = check
		if check.Status != "healthy" {
			overallStatus = "unhealthy"
		}
	}

	// Update status
	hc.status.Status = overallStatus
	hc.status.Timestamp = time.Now()
	hc.status.Checks = checks
}

// handleHealthCheck handles HTTP health check requests
func (hc *Checker) handleHealthCheck(w http.ResponseWriter, _ *http.Request) {
	hc.mu.RLock()
	status := hc.snapshotStatus()
	hc.mu.RUnlock()

	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	// Set status code based on health
	if status.Status == "healthy" {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	// Encode response
	if err := json.NewEncoder(w).Encode(status); err != nil {
		klog.Errorf("Failed to encode health check response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// handleNotFound handles requests to unknown endpoints
func (hc *Checker) handleNotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"error": "Not found",
		"path":  r.URL.Path,
	}); err != nil {
		klog.Errorf("Failed to encode not found response: %v", err)
	}
}

// snapshotStatus returns a deep copy of the current status.
// Must be called with hc.mu held (at least RLock).
func (hc *Checker) snapshotStatus() *Status {
	cp := *hc.status
	cp.Checks = make(map[string]Check, len(hc.status.Checks))
	for k, v := range hc.status.Checks {
		cp.Checks[k] = v
	}
	return &cp
}

// GetStatus returns the current health status
func (hc *Checker) GetStatus() Status {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return *hc.snapshotStatus()
}

// SetStatus sets the overall health status
func (hc *Checker) SetStatus(status string) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.status.Status = status
	hc.status.Timestamp = time.Now()
}

// Built-in health check functions

// BasicHealthCheck provides a basic health check
func BasicHealthCheck() CheckFunc {
	return func(_ context.Context) Check {
		return Check{
			Status:  "healthy",
			Message: "Basic health check passed",
		}
	}
}

// MemoryHealthCheck provides a memory usage health check
func MemoryHealthCheck() CheckFunc {
	return func(_ context.Context) Check {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)

		// Consider unhealthy if memory usage is too high
		if m.Alloc > 1<<30 { // 1GB
			return Check{
				Status:  "unhealthy",
				Message: "Memory usage too high",
				Details: map[string]interface{}{
					"alloc":     m.Alloc,
					"sys":       m.Sys,
					"heapAlloc": m.HeapAlloc,
				},
			}
		}

		return Check{
			Status:  "healthy",
			Message: "Memory usage normal",
			Details: map[string]interface{}{
				"alloc":     m.Alloc,
				"sys":       m.Sys,
				"heapAlloc": m.HeapAlloc,
			},
		}
	}
}
