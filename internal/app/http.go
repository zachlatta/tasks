package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/tasks/internal/auth"
)

func NewHTTPHandler(
	web http.Handler,
	oauth *auth.Server,
	mcpServer *mcp.Server,
	taskAPI http.Handler,
	publicURL string,
	logger *slog.Logger,
) (http.Handler, error) {
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
	mux.Handle("/api/tools/", oauth.RequireSharedSecretBearer(taskAPI))
	mux.Handle("/", web)
	return logRequests(logger, mux), nil
}

// logRequests writes one line per request: method, path, status, and duration.
// Two deliberate omissions: successful health probes, because a 30-second
// Docker healthcheck would drown everything else, and the query string,
// because OAuth parameters travel there.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		if r.URL.Path == "/healthz" && status < 400 {
			return
		}
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

// statusRecorder captures the response status for the request log while
// passing everything else through to the underlying writer.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

// Unwrap lets http.ResponseController reach the underlying writer's optional
// interfaces (flushing, deadlines) through the recorder.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Flush keeps direct http.Flusher type assertions working for streaming
// responses.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
