package taskapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/zachlatta/tasks/internal/task"
)

const maxRequestBytes = 10 << 20

// NewHandler exposes the shared task tools at POST /api/tools/{name}. The
// caller is responsible for wrapping this handler in authentication.
func NewHandler(tools *Tools) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tools/{name}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case QueryTasksSQLTool:
			invoke(w, r, tools.QueryTasksSQL)
		case CreateTaskTool:
			invokeMutation(w, r, tools.CreateTask)
		case EditTaskTextTool:
			invokeMutation(w, r, tools.EditTaskText)
		case UpdateTaskTool:
			invokeMutation(w, r, tools.UpdateTask)
		case CompleteTaskTool:
			invokeMutation(w, r, tools.CompleteTask)
		case DeleteTaskTool:
			invokeMutation(w, r, tools.DeleteTask)
		case RestoreTaskTool:
			invokeMutation(w, r, tools.RestoreTask)
		default:
			writeError(w, http.StatusNotFound, "tool_not_found", "no task tool named "+r.PathValue("name"))
		}
	})
	return mux
}

func invokeMutation[Input, Output any](w http.ResponseWriter, r *http.Request, operation func(context.Context, Input) (Output, error)) {
	invoke(w, r, func(ctx context.Context, input Input) (Output, error) {
		return operation(task.WithAuditMetadata(ctx, task.AuditMetadata{
			ActorKind: "shared_secret",
			Source:    "cli",
		}), input)
	})
}

func invoke[Input, Output any](w http.ResponseWriter, r *http.Request, operation func(context.Context, Input) (Output, error)) {
	var input Input
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "request body is not valid JSON: "+err.Error())
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_input", "request body must contain one JSON value")
		return
	}
	output, err := operation(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "tool_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": output})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": message,
	}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
