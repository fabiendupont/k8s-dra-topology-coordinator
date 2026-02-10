// Package health provides tests for health check functionality
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewChecker(t *testing.T) {
	hc := NewChecker(8080, "1.0.0")

	if hc.port != 8080 {
		t.Errorf("Expected port 8080, got %d", hc.port)
	}

	if hc.version != "1.0.0" {
		t.Errorf("Expected version 1.0.0, got %s", hc.version)
	}

	if hc.status.Status != statusHealthy {
		t.Errorf("Expected initial status 'healthy', got %s", hc.status.Status)
	}
}

func TestCheckerStartStop(t *testing.T) {
	hc := NewChecker(0, "1.0.0") // Use default port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start health checker
	err := hc.Start(ctx)
	if err != nil {
		t.Errorf("Failed to start health checker: %v", err)
	}

	// Wait a bit for server to start
	time.Sleep(100 * time.Millisecond)

	// Stop health checker
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()

	err = hc.Stop(stopCtx)
	if err != nil {
		t.Errorf("Failed to stop health checker: %v", err)
	}
}

func TestHealthCheckEndpoint(t *testing.T) {
	hc := NewChecker(0, "1.0.0")
	hc.AddCheck("basic", BasicHealthCheck())
	hc.AddCheck("memory", MemoryHealthCheck())

	// Create test request
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	// Handle request
	hc.handleHealthCheck(w, req)

	// Check response
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	// Parse response
	var status Status
	err := json.NewDecoder(w.Body).Decode(&status)
	if err != nil {
		t.Errorf("Failed to decode response: %v", err)
	}

	if status.Status != statusHealthy {
		t.Errorf("Expected status 'healthy', got %s", status.Status)
	}

	if status.Version != "1.0.0" {
		t.Errorf("Expected version '1.0.0', got %s", status.Version)
	}
}

func TestNotFoundEndpoint(t *testing.T) {
	hc := NewChecker(0, "1.0.0")

	// Create test request
	req := httptest.NewRequest("GET", "/nonexistent", nil)
	w := httptest.NewRecorder()

	// Handle request
	hc.handleNotFound(w, req)

	// Check response
	if w.Code != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", w.Code)
	}

	// Parse response
	var response map[string]string
	err := json.NewDecoder(w.Body).Decode(&response)
	if err != nil {
		t.Errorf("Failed to decode response: %v", err)
	}

	if response["error"] != "Not found" {
		t.Errorf("Expected error 'Not found', got %s", response["error"])
	}
}

func TestBasicHealthCheck(t *testing.T) {
	check := BasicHealthCheck()
	result := check(context.Background())

	if result.Status != statusHealthy {
		t.Errorf("Expected status 'healthy', got %s", result.Status)
	}

	if result.Message != "Basic health check passed" {
		t.Errorf("Expected message 'Basic health check passed', got %s", result.Message)
	}
}

func TestMemoryHealthCheck(t *testing.T) {
	check := MemoryHealthCheck()
	result := check(context.Background())

	if result.Status != statusHealthy && result.Status != statusUnhealthy {
		t.Errorf("Expected status 'healthy' or 'unhealthy', got %s", result.Status)
	}

	if result.Details == nil {
		t.Error("Expected memory details to be present")
	}

	// Check for required memory fields
	requiredFields := []string{"alloc", "sys", "heapAlloc"}
	for _, field := range requiredFields {
		if _, exists := result.Details[field]; !exists {
			t.Errorf("Expected memory detail field '%s' to be present", field)
		}
	}
}

func TestCheckerConcurrency(t *testing.T) {
	hc := NewChecker(0, "1.0.0")

	// Test concurrent access
	done := make(chan bool)

	// Start multiple goroutines accessing the health checker
	for i := 0; i < 10; i++ {
		go func() {
			status := hc.GetStatus()
			if status.Status != statusHealthy {
				t.Errorf("Expected status 'healthy', got %s", status.Status)
			}
			done <- true
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestCheckerStatusUpdates(t *testing.T) {
	hc := NewChecker(0, "1.0.0")

	// Test status updates
	hc.SetStatus(statusUnhealthy)
	status := hc.GetStatus()
	if status.Status != statusUnhealthy {
		t.Errorf("Expected status 'unhealthy', got %s", status.Status)
	}

	hc.SetStatus(statusHealthy)
	status = hc.GetStatus()
	if status.Status != statusHealthy {
		t.Errorf("Expected status 'healthy', got %s", status.Status)
	}
}

// Benchmark tests for performance
func BenchmarkHealthCheckEndpoint(b *testing.B) {
	hc := NewChecker(0, "1.0.0")
	hc.AddCheck("basic", BasicHealthCheck())
	hc.AddCheck("memory", MemoryHealthCheck())

	req := httptest.NewRequest("GET", "/healthz", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		hc.handleHealthCheck(w, req)
	}
}

func BenchmarkMemoryHealthCheck(b *testing.B) {
	check := MemoryHealthCheck()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		check(context.Background())
	}
}

func TestPerformHealthChecks(t *testing.T) {
	hc := NewChecker(0, "1.0.0")
	hc.AddCheck("basic", BasicHealthCheck())
	hc.AddCheck("memory", MemoryHealthCheck())

	ctx := context.Background()

	// Call performHealthChecks directly
	hc.performHealthChecks(ctx)

	// Check that status was updated
	status := hc.GetStatus()
	if status.Status != statusHealthy {
		t.Errorf("Expected status 'healthy', got %s", status.Status)
	}

	if len(status.Checks) != 2 {
		t.Errorf("Expected 2 checks, got %d", len(status.Checks))
	}
}

func TestPerformHealthChecksWithUnhealthyCheck(t *testing.T) {
	hc := NewChecker(0, "1.0.0")

	// Add an unhealthy check
	unhealthyCheck := func(_ context.Context) Check {
		return Check{
			Status:  statusUnhealthy,
			Message: "Test failure",
		}
	}
	hc.AddCheck("unhealthy", unhealthyCheck)
	hc.AddCheck("basic", BasicHealthCheck())

	ctx := context.Background()

	// Call performHealthChecks directly
	hc.performHealthChecks(ctx)

	// Check that overall status is unhealthy
	status := hc.GetStatus()
	if status.Status != statusUnhealthy {
		t.Errorf("Expected overall status 'unhealthy', got %s", status.Status)
	}
}

func TestHandleHealthCheckUnhealthy(t *testing.T) {
	hc := NewChecker(0, "1.0.0")

	// Set status to unhealthy
	hc.SetStatus(statusUnhealthy)

	// Create test request
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	// Handle request
	hc.handleHealthCheck(w, req)

	// Check response code - should be 503 for unhealthy
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", w.Code)
	}
}

func TestUpdateStatus(t *testing.T) {
	hc := NewChecker(0, "1.0.0")

	// Add a check
	hc.AddCheck("basic", BasicHealthCheck())

	// Create a context that's already cancelled so updateStatus returns immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Call updateStatus - it should return immediately due to cancelled context
	hc.updateStatus(ctx)

	// Status should still be healthy since the loop exited before any checks ran
	status := hc.GetStatus()
	if status.Status != statusHealthy {
		t.Errorf("Expected status 'healthy', got %s", status.Status)
	}
}
