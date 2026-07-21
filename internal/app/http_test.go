package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/task-tracker/internal/auth"
)

func TestHTTPHandlerExposesDiscoveryAndProtectsMCP(t *testing.T) {
	t.Parallel()

	oauth := auth.NewServer(auth.Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	handler, err := NewHTTPHandler(http.NotFoundHandler(), oauth, mcpServer, "https://tasks.example.com")
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
}
