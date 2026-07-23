package app

import (
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/tasks/internal/auth"
)

func NewHTTPHandler(web http.Handler, oauth *auth.Server, mcpServer *mcp.Server, publicURL string) (http.Handler, error) {
	mux := http.NewServeMux()
	oauth.RegisterRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	crossOrigin := http.NewCrossOriginProtection()
	if err := crossOrigin.AddTrustedOrigin(publicURL); err != nil {
		return nil, fmt.Errorf("configure MCP origin protection: %w", err)
	}
	mux.Handle("/mcp", crossOrigin.Handler(oauth.RequireBearer(mcpHandler)))
	mux.Handle("/", web)
	return mux, nil
}
