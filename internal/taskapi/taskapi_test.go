package taskapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/tasks/internal/postgres"
	"github.com/zachlatta/tasks/internal/task"
	"github.com/zachlatta/tasks/internal/tasktest"
)

type readerFunc func(context.Context, string) (postgres.Result, error)

func (function readerFunc) Query(ctx context.Context, statement string) (postgres.Result, error) {
	return function(ctx, statement)
}

type auditRepository struct {
	inner     *tasktest.Repository
	mutations []task.AuditMetadata
}

func newAuditRepository() *auditRepository {
	return &auditRepository{inner: tasktest.NewRepository()}
}

func (r *auditRepository) Create(ctx context.Context, item task.Task) error {
	r.mutations = append(r.mutations, task.AuditMetadataFromContext(ctx))
	return r.inner.Create(ctx, item)
}

func (r *auditRepository) Update(ctx context.Context, item task.Task) error {
	r.mutations = append(r.mutations, task.AuditMetadataFromContext(ctx))
	return r.inner.Update(ctx, item)
}

func (r *auditRepository) Get(ctx context.Context, id string) (task.Task, error) {
	return r.inner.Get(ctx, id)
}

func (r *auditRepository) List(ctx context.Context) ([]task.Task, error) {
	return r.inner.List(ctx)
}

func TestToolsAreSharedTaskOperations(t *testing.T) {
	t.Parallel()

	repository := tasktest.NewRepository()
	service := task.NewService(repository, time.Now, func() string { return "shared-task" })
	tools := NewTools(service, readerFunc(func(_ context.Context, statement string) (postgres.Result, error) {
		if statement != "SELECT id FROM tasks" {
			t.Fatalf("query = %q", statement)
		}
		return postgres.Result{Columns: []string{"id"}, Rows: []map[string]any{{"id": "shared-task"}}}, nil
	}))

	ctx := task.WithAuditMetadata(context.Background(), task.AuditMetadata{ActorKind: "shared_secret", Source: "cli"})
	created, err := tools.CreateTask(ctx, CreateTaskInput{Title: "Shared operation"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	title := "Updated operation"
	updated, err := tools.UpdateTask(ctx, UpdateTaskInput{ID: created.ID, Title: &title})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Title != title || updated.Version != 2 {
		t.Fatalf("updated task = %#v", updated)
	}
	result, err := tools.QueryTasksSQL(ctx, SQLQueryInput{SQL: "SELECT id FROM tasks"})
	if err != nil {
		t.Fatalf("QueryTasksSQL: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["id"] != "shared-task" {
		t.Fatalf("query result = %#v", result)
	}
	completed, err := tools.CompleteTask(ctx, CompleteTaskInput{ID: created.ID})
	if err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	if completed.Status != task.StatusDone {
		t.Fatalf("completed task = %#v", completed)
	}
}

func TestDeleteAndRestoreTaskThroughToolsAndHandler(t *testing.T) {
	t.Parallel()

	repository := newAuditRepository()
	service := task.NewService(repository, time.Now, func() string { return "disposable" })
	tools := NewTools(service, readerFunc(func(context.Context, string) (postgres.Result, error) {
		return postgres.Result{}, nil
	}))
	handler := NewHandler(tools)

	if _, err := tools.CreateTask(context.Background(), CreateTaskInput{Title: "Disposable"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, httptest.NewRequest(
		http.MethodPost, "/api/tools/delete_task", strings.NewReader(`{"id":"disposable"}`),
	))
	if deleteResponse.Code != http.StatusOK || !strings.Contains(deleteResponse.Body.String(), `"deleted_at"`) {
		t.Fatalf("delete response = %d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if _, err := service.Get(context.Background(), "disposable"); !errors.Is(err, task.ErrNotFound) {
		t.Fatalf("deleted task is still readable: %v", err)
	}
	if len(repository.mutations) != 2 ||
		repository.mutations[1].Action != "delete" ||
		repository.mutations[1].ActorKind != "shared_secret" ||
		repository.mutations[1].Source != "cli" {
		t.Fatalf("delete audit metadata = %#v", repository.mutations)
	}

	restoreResponse := httptest.NewRecorder()
	handler.ServeHTTP(restoreResponse, httptest.NewRequest(
		http.MethodPost, "/api/tools/restore_task", strings.NewReader(`{"id":"disposable"}`),
	))
	if restoreResponse.Code != http.StatusOK || strings.Contains(restoreResponse.Body.String(), `"deleted_at"`) {
		t.Fatalf("restore response = %d %s", restoreResponse.Code, restoreResponse.Body.String())
	}
	restored, err := service.Get(context.Background(), "disposable")
	if err != nil {
		t.Fatalf("get restored task: %v", err)
	}
	if restored.Title != "Disposable" || restored.Deleted() {
		t.Fatalf("restored task = %#v", restored)
	}
	if repository.mutations[2].Action != "restore" {
		t.Fatalf("restore audit metadata = %#v", repository.mutations[2])
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(
		http.MethodPost, "/api/tools/delete_task", strings.NewReader(`{"id":"nobody"}`),
	))
	if missing.Code != http.StatusUnprocessableEntity {
		t.Fatalf("delete missing task response = %d %s", missing.Code, missing.Body.String())
	}
}

func TestHandlerInvokesToolsAndReturnsEnvelopes(t *testing.T) {
	t.Parallel()

	repository := newAuditRepository()
	service := task.NewService(repository, time.Now, func() string { return "api-task" })
	tools := NewTools(service, readerFunc(func(_ context.Context, statement string) (postgres.Result, error) {
		if strings.HasPrefix(statement, "DELETE") {
			return postgres.Result{}, errors.New("only read-only queries are allowed")
		}
		return postgres.Result{Columns: []string{"answer"}, Rows: []map[string]any{{"answer": int64(42)}}}, nil
	}))
	handler := NewHandler(tools)

	create := httptest.NewRequest(http.MethodPost, "/api/tools/create_task", strings.NewReader(`{"title":"From API"}`))
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK || !strings.Contains(createResponse.Body.String(), `"id":"api-task"`) {
		t.Fatalf("create response = %d %s", createResponse.Code, createResponse.Body.String())
	}
	stored, err := service.Get(context.Background(), "api-task")
	if err != nil {
		t.Fatalf("get created task: %v", err)
	}
	if stored.Title != "From API" {
		t.Fatalf("stored task = %#v", stored)
	}
	if len(repository.mutations) != 1 ||
		repository.mutations[0].ActorKind != "shared_secret" ||
		repository.mutations[0].Source != "cli" {
		t.Fatalf("API mutation audit metadata = %#v", repository.mutations)
	}

	query := httptest.NewRequest(http.MethodPost, "/api/tools/query_tasks_sql", strings.NewReader(`{"sql":"SELECT 42 AS answer"}`))
	queryResponse := httptest.NewRecorder()
	handler.ServeHTTP(queryResponse, query)
	if queryResponse.Code != http.StatusOK || !strings.Contains(queryResponse.Body.String(), `"answer":42`) {
		t.Fatalf("query response = %d %s", queryResponse.Code, queryResponse.Body.String())
	}

	write := httptest.NewRequest(http.MethodPost, "/api/tools/query_tasks_sql", strings.NewReader(`{"sql":"DELETE FROM tasks"}`))
	writeResponse := httptest.NewRecorder()
	handler.ServeHTTP(writeResponse, write)
	if writeResponse.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(writeResponse.Body.String(), `"code":"tool_error"`) ||
		!strings.Contains(writeResponse.Body.String(), "only read-only") {
		t.Fatalf("write response = %d %s", writeResponse.Code, writeResponse.Body.String())
	}

	malformed := httptest.NewRequest(http.MethodPost, "/api/tools/create_task", strings.NewReader(`{`))
	malformedResponse := httptest.NewRecorder()
	handler.ServeHTTP(malformedResponse, malformed)
	if malformedResponse.Code != http.StatusBadRequest || !strings.Contains(malformedResponse.Body.String(), `"code":"invalid_input"`) {
		t.Fatalf("malformed response = %d %s", malformedResponse.Code, malformedResponse.Body.String())
	}
}

func TestClientUsesSharedSecretAndTypedToolContract(t *testing.T) {
	t.Parallel()

	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer same-secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tools/create_task":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"title":"Created"`) {
				t.Errorf("create body = %s", body)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"remote","title":"Created","status":"todo","version":1}}`)
		case "/api/tools/update_task":
			_, _ = io.WriteString(w, `{"data":{"id":"remote","title":"Updated","status":"todo","version":2}}`)
		case "/api/tools/query_tasks_sql":
			_, _ = io.WriteString(w, `{"data":{"columns":["id"],"rows":[{"id":"remote"}],"truncated":false}}`)
		case "/api/tools/complete_task":
			_, _ = io.WriteString(w, `{"data":{"id":"remote","title":"Updated","status":"done","version":3}}`)
		case "/api/tools/delete_task":
			_, _ = io.WriteString(w, `{"data":{"id":"remote","title":"Updated","status":"done","version":4,"deleted_at":"2026-07-24T12:00:00Z"}}`)
		case "/api/tools/restore_task":
			_, _ = io.WriteString(w, `{"data":{"id":"remote","title":"Updated","status":"done","version":5}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(server.URL+"/", "same-secret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	created, err := client.CreateTask(context.Background(), CreateTaskInput{Title: "Created"})
	if err != nil || created.ID != "remote" {
		t.Fatalf("CreateTask = %#v, %v", created, err)
	}
	title := "Updated"
	updated, err := client.UpdateTask(context.Background(), UpdateTaskInput{ID: created.ID, Title: &title})
	if err != nil || updated.Version != 2 {
		t.Fatalf("UpdateTask = %#v, %v", updated, err)
	}
	result, err := client.QueryTasksSQL(context.Background(), SQLQueryInput{SQL: "SELECT id FROM tasks"})
	if err != nil || len(result.Rows) != 1 {
		t.Fatalf("QueryTasksSQL = %#v, %v", result, err)
	}
	completed, err := client.CompleteTask(context.Background(), CompleteTaskInput{ID: created.ID})
	if err != nil || completed.Status != task.StatusDone {
		t.Fatalf("CompleteTask = %#v, %v", completed, err)
	}
	deleted, err := client.DeleteTask(context.Background(), DeleteTaskInput{ID: created.ID})
	if err != nil || !deleted.Deleted() {
		t.Fatalf("DeleteTask = %#v, %v", deleted, err)
	}
	restored, err := client.RestoreTask(context.Background(), RestoreTaskInput{ID: created.ID})
	if err != nil || restored.Deleted() {
		t.Fatalf("RestoreTask = %#v, %v", restored, err)
	}
	if len(paths) != 6 {
		t.Fatalf("paths = %v", paths)
	}
}

func TestClientReturnsStructuredAPIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"tool_error","message":"task is blocked"}}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "same-secret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.CompleteTask(context.Background(), CompleteTaskInput{ID: "blocked"})
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("error = %v, want APIError", err)
	}
	if apiError.Status != http.StatusUnprocessableEntity ||
		apiError.Code != "tool_error" ||
		apiError.Message != "task is blocked" {
		t.Fatalf("APIError = %#v", apiError)
	}
}

func TestClientRejectsUnsafeOrIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		apiURL string
		secret string
	}{
		"missing URL":       {"", "secret"},
		"missing secret":    {"https://tasks.example.com", ""},
		"non HTTP scheme":   {"postgres://tasks.example.com/tasks", "secret"},
		"insecure remote":   {"http://tasks.example.com", "secret"},
		"URL with query":    {"https://tasks.example.com?private=value", "secret"},
		"URL with fragment": {"https://tasks.example.com#fragment", "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(test.apiURL, test.secret); err == nil {
				t.Fatalf("NewClient(%q, ...) succeeded", test.apiURL)
			}
		})
	}
}

func TestHandlerRejectsUnknownTool(t *testing.T) {
	t.Parallel()

	tools := NewTools(
		task.NewService(tasktest.NewRepository(), time.Now, func() string { return "unused" }),
		readerFunc(func(context.Context, string) (postgres.Result, error) { return postgres.Result{}, nil }),
	)
	request := httptest.NewRequest(http.MethodPost, "/api/tools/not_a_tool", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	NewHandler(tools).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Error.Code != "tool_not_found" {
		t.Fatalf("error code = %q", envelope.Error.Code)
	}
}

func TestUpdateTaskSetsAndClearsTheWait(t *testing.T) {
	t.Parallel()

	repo := tasktest.NewRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := task.NewService(repo, func() time.Time { return now }, func() string { return "licence-signoff" })
	tools := NewTools(service, readerFunc(func(context.Context, string) (postgres.Result, error) {
		return postgres.Result{}, nil
	}))
	created, err := tools.CreateTask(context.Background(), CreateTaskInput{Title: "Priya, Tom & Rae sign off"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	for _, supplied := range []struct {
		name string
		raw  string
		want time.Time
	}{
		{name: "date only", raw: "2026-07-27", want: time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)},
		{name: "timestamp", raw: "2026-07-28T13:30:00Z", want: time.Date(2026, time.July, 28, 13, 30, 0, 0, time.UTC)},
		{name: "offset timestamp", raw: "2026-07-29T09:00:00-04:00", want: time.Date(2026, time.July, 29, 13, 0, 0, 0, time.UTC)},
	} {
		waitingOn := "Priya, Tom & Rae"
		edited, err := tools.UpdateTask(context.Background(), UpdateTaskInput{
			ID: created.ID, WakeAt: &supplied.raw, WaitingOn: &waitingOn,
		})
		if err != nil {
			t.Fatalf("UpdateTask with a %s wake date: %v", supplied.name, err)
		}
		if edited.WakeAt == nil || !edited.WakeAt.Equal(supplied.want) {
			t.Fatalf("%s wake date = %v, want %v", supplied.name, edited.WakeAt, supplied.want)
		}
		if edited.WaitingOn != waitingOn {
			t.Fatalf("%s waiting on = %q", supplied.name, edited.WaitingOn)
		}
	}

	empty := ""
	awake, err := tools.UpdateTask(context.Background(), UpdateTaskInput{
		ID: created.ID, WakeAt: &empty, WaitingOn: &empty,
	})
	if err != nil {
		t.Fatalf("UpdateTask clearing the wait: %v", err)
	}
	if awake.WakeAt != nil || awake.WaitingOn != "" {
		t.Fatalf("clearing the wait left %#v", awake)
	}
}

func TestUpdateTaskRejectsAnUnparsableWakeDate(t *testing.T) {
	t.Parallel()

	repo := tasktest.NewRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := task.NewService(repo, func() time.Time { return now }, func() string { return "bad-date" })
	tools := NewTools(service, readerFunc(func(context.Context, string) (postgres.Result, error) {
		return postgres.Result{}, nil
	}))
	created, err := tools.CreateTask(context.Background(), CreateTaskInput{Title: "Bad date"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	raw := "next tuesday"
	if _, err := tools.UpdateTask(context.Background(), UpdateTaskInput{ID: created.ID, WakeAt: &raw}); !errors.Is(err, task.ErrInvalid) {
		t.Fatalf("UpdateTask error = %v, want ErrInvalid", err)
	}
}

func TestCreateTaskAcceptsAWait(t *testing.T) {
	t.Parallel()

	repo := tasktest.NewRepository()
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	service := task.NewService(repo, func() time.Time { return now }, func() string { return "waiting-from-birth" })
	tools := NewTools(service, readerFunc(func(context.Context, string) (postgres.Result, error) {
		return postgres.Result{}, nil
	}))
	created, err := tools.CreateTask(context.Background(), CreateTaskInput{
		Title: "Chase the signed contract", WakeAt: "2026-08-03", WaitingOn: "the vendor",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	want := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
	if created.WakeAt == nil || !created.WakeAt.Equal(want) {
		t.Fatalf("wake date = %v, want %v", created.WakeAt, want)
	}
	if created.WaitingOn != "the vendor" || created.SnoozeCount != 1 {
		t.Fatalf("created wait = %q, snoozes = %d", created.WaitingOn, created.SnoozeCount)
	}
	if !created.Snoozed(now) {
		t.Fatal("a task created with a future wake date should start asleep")
	}
}
