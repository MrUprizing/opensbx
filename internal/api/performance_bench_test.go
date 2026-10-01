package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"opensbx/internal/api"
	"opensbx/internal/sandbox"
)

const performanceAPIKey = "benchmark-only-key"

// performanceApplication supplies domain values directly, avoiding a test-side
// DTO conversion that would be incorrectly included in the API benchmark.
type performanceApplication struct {
	sandbox.Application
	items    []sandbox.Summary
	detail   sandbox.Detail
	commands []sandbox.Command
}

func (a *performanceApplication) Ping(context.Context) error { return nil }
func (a *performanceApplication) List(context.Context) ([]sandbox.Summary, error) {
	return a.items, nil
}
func (a *performanceApplication) Inspect(_ context.Context, id sandbox.SandboxID) (sandbox.Detail, error) {
	result := a.detail
	result.ID = id
	result.Summary.ID = id
	return result, nil
}
func (a *performanceApplication) ListCommands(context.Context, sandbox.SandboxID) ([]sandbox.Command, error) {
	return a.commands, nil
}

func performanceRouter(size int) (*gin.Engine, string) {
	app := &performanceApplication{
		items:    make([]sandbox.Summary, size),
		commands: make([]sandbox.Command, size),
		detail: sandbox.Detail{Summary: sandbox.Summary{
			Name: "benchmark-sandbox", Image: "node:25-alpine", Status: "running", State: "running",
			Ports: []sandbox.Port{{Number: 3000, Protocol: "tcp"}}, URL: "http://benchmark.localhost:40000",
		}, Running: true, Resources: sandbox.ResourceLimits{MemoryMB: 128, CPUs: 1}},
	}
	for i := 0; i < size; i++ {
		id := sandbox.SandboxID(fmt.Sprintf("sbx-%032x", i+1))
		app.items[i] = sandbox.Summary{ID: id, Name: fmt.Sprintf("sandbox-%04d", i), Image: "node:25-alpine", Status: "running", State: "running"}
		app.commands[i] = sandbox.Command{ID: sandbox.CommandID(fmt.Sprintf("cmd_%032x", i+1)), SandboxID: "sbx-benchmark", Name: "node", Args: []string{"-e", "process.exit(0)"}, StartedAt: int64(i + 1)}
	}
	router := gin.New()
	handler := api.New(app)
	handler.RegisterHealthCheck(router)
	v1 := router.Group("/v1")
	v1.Use(api.APIKeyAuth(performanceAPIKey))
	handler.RegisterRoutes(v1)
	return router, "sbx-benchmark"
}

func BenchmarkAuthenticatedAPIListSandboxesHTTP(b *testing.B) {
	for _, size := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("httptest/items_%d", size), func(b *testing.B) {
			router, _ := performanceRouter(size)
			benchmarkAuthenticatedRequest(b, router, http.MethodGet, "/v1/sandboxes")
		})
	}
}

func BenchmarkAuthenticatedAPIInspectSandboxHTTP(b *testing.B) {
	router, id := performanceRouter(1)
	benchmarkAuthenticatedRequest(b, router, http.MethodGet, "/v1/sandboxes/"+id)
}

func BenchmarkAuthenticatedAPICommandHistoryHTTP(b *testing.B) {
	for _, size := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("httptest/commands_%d", size), func(b *testing.B) {
			router, id := performanceRouter(size)
			benchmarkAuthenticatedRequest(b, router, http.MethodGet, "/v1/sandboxes/"+id+"/cmd")
		})
	}
}

func benchmarkAuthenticatedRequest(b *testing.B, router http.Handler, method, path string) {
	b.Helper()
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer "+performanceAPIKey)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("%s %s status=%d", method, path, response.Code)
		}
	}
}
