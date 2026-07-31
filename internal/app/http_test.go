package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/tasks/internal/auth"
)

func TestHTTPHandlerExposesDiscoveryAndProtectsMCP(t *testing.T) {
	t.Parallel()

	oauth := auth.NewServer(auth.Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	taskAPI := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler, err := NewHTTPHandler(http.NotFoundHandler(), oauth, mcpServer, taskAPI, "https://tasks.example.com", nil)
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}

	metadataRequest := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	metadataResponse := httptest.NewRecorder()
	handler.ServeHTTP(metadataResponse, metadataRequest)
	if metadataResponse.Code != http.StatusOK || !strings.Contains(metadataResponse.Body.String(), `"resource":"https://tasks.example.com/mcp"`) {
		t.Fatalf("metadata response = %d %s", metadataResponse.Code, metadataResponse.Body.String())
	}

	mcpRequest := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	mcpResponse := httptest.NewRecorder()
	handler.ServeHTTP(mcpResponse, mcpRequest)
	if mcpResponse.Code != http.StatusUnauthorized || !strings.Contains(mcpResponse.Header().Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("MCP response = %d, WWW-Authenticate %q", mcpResponse.Code, mcpResponse.Header().Get("WWW-Authenticate"))
	}

	crossOrigin := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	crossOrigin.Header.Set("Origin", "https://attacker.example.com")
	crossOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(crossOriginResponse, crossOrigin)
	if crossOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want %d", crossOriginResponse.Code, http.StatusForbidden)
	}

	apiRequest := httptest.NewRequest(http.MethodPost, "/api/tools/create_task", strings.NewReader(`{"title":"API task"}`))
	apiResponse := httptest.NewRecorder()
	handler.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status = %d, want %d", apiResponse.Code, http.StatusUnauthorized)
	}

	apiRequest = httptest.NewRequest(http.MethodPost, "/api/tools/create_task", strings.NewReader(`{"title":"API task"}`))
	apiRequest.Header.Set("Authorization", "Bearer secret")
	apiResponse = httptest.NewRecorder()
	handler.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != http.StatusNoContent {
		t.Fatalf("authenticated API status = %d, want %d; body = %s", apiResponse.Code, http.StatusNoContent, apiResponse.Body.String())
	}
}
