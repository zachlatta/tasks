package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/tasks/internal/auth"
)

// A week of production logs contained nothing but startup lines: every
// request, error included, was invisible. The handler now writes one log line
// per request — method, path, status, duration — while successful health
// probes stay quiet so a 30-second Docker healthcheck does not drown the log.
func TestHTTPHandlerLogsRequests(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))
	oauth := auth.NewServer(auth.Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	taskAPI := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler, err := NewHTTPHandler(http.NotFoundHandler(), oauth, mcpServer, taskAPI, "https://tasks.example.com", logger)
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}

	serve := func(method, path string, header http.Header) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, nil)
		for name, values := range header {
			request.Header[name] = values
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	// A successful health probe stays out of the log.
	if response := serve(http.MethodGet, "/healthz", nil); response.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", response.Code)
	}
	if buffer.Len() != 0 {
		t.Fatalf("healthz logged: %s", buffer.String())
	}

	// An unauthenticated MCP request is a 401 worth seeing.
	if response := serve(http.MethodPost, "/mcp", nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("mcp status = %d", response.Code)
	}
	line := buffer.String()
	for _, want := range []string{"method=POST", "path=/mcp", "status=401", "duration_ms="} {
		if !strings.Contains(line, want) {
			t.Errorf("401 log line %q is missing %q", line, want)
		}
	}

	// Ordinary traffic is logged too; the query string is not, because OAuth
	// parameters travel there.
	buffer.Reset()
	if response := serve(http.MethodGet, "/some-page?secret-ish=value", nil); response.Code != http.StatusNotFound {
		t.Fatalf("page status = %d", response.Code)
	}
	line = buffer.String()
	if !strings.Contains(line, "path=/some-page") || !strings.Contains(line, "status=404") {
		t.Errorf("404 log line = %q", line)
	}
	if strings.Contains(line, "secret-ish") {
		t.Errorf("log line %q leaks the query string", line)
	}
}

// A nil logger keeps the handler silent, which the tests of other packages
// rely on.
func TestHTTPHandlerAllowsNilLogger(t *testing.T) {
	t.Parallel()

	oauth := auth.NewServer(auth.Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	handler, err := NewHTTPHandler(http.NotFoundHandler(), oauth, mcpServer, http.NotFoundHandler(), "https://tasks.example.com", nil)
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}
