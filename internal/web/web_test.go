package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zachlatta/tasks/internal/auth"
	"github.com/zachlatta/tasks/internal/objectstore"
	"github.com/zachlatta/tasks/internal/task"
	"github.com/zachlatta/tasks/internal/tasktest"
)

func TestLoginProtectsTaskPage(t *testing.T) {
	t.Parallel()

	handler, service := testHandler(t)
	created, err := service.Create(context.Background(), task.CreateInput{Title: "Visible after login"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/", nil))
	if unauthorized.Code != http.StatusSeeOther || unauthorized.Header().Get("Location") != "/login" {
		t.Fatalf("unauthorized response = %d %q", unauthorized.Code, unauthorized.Header().Get("Location"))
	}

	wrong := postForm(handler, "/login", url.Values{"secret": {"wrong"}}, nil)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret status = %d, want %d", wrong.Code, http.StatusUnauthorized)
	}

	login := postForm(handler, "/login", url.Values{"secret": {"shared-secret"}}, nil)
	if login.Code != http.StatusSeeOther || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login response = %d, cookies = %#v", login.Code, login.Result().Cookies())
	}
	if cookie := login.Result().Cookies()[0]; cookie.Name != "tasks_session" {
		t.Fatalf("session cookie = %q, want tasks_session", cookie.Name)
	}
	page := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(login.Result().Cookies()[0])
	handler.ServeHTTP(page, request)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), created.Title) {
		t.Fatalf("task page status = %d; body: %s", page.Code, page.Body.String())
	}
	if body := page.Body.String(); !strings.Contains(body, "<title>Tasks</title>") {
		t.Fatalf("index page identity is stale; body: %s", body)
	}
	// The page must describe the real storage backend, not the pre-migration one.
	if body := page.Body.String(); !strings.Contains(body, "POSTGRES-BACKED") || strings.Contains(body, "MARKDOWN-BACKED") {
		t.Fatalf("index page storage label is stale; body: %s", body)
	}
}

func TestTaskDescriptionsRenderSafeMarkdown(t *testing.T) {
	t.Parallel()

	handler, service := testHandler(t)
	_, err := service.Create(context.Background(), task.CreateInput{
		Title: "Markdown task",
		Description: `## Launch plan

- render **bold** text
- visit [example](https://example.com)

<script>alert("not safe")</script>

[bad link](javascript:alert)`,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	cookie, _ := login(t, handler)

	page := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	handler.ServeHTTP(page, request)
	body := page.Body.String()
	for _, want := range []string{
		`<div class="description">`,
		`<h2>Launch plan</h2>`,
		`<li>render <strong>bold</strong> text</li>`,
		`href="https://example.com"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("task page does not contain rendered Markdown %q; body: %s", want, body)
		}
	}
	for _, unsafe := range []string{`<script>`, `href="javascript:`} {
		if strings.Contains(body, unsafe) {
			t.Errorf("task page contains unsafe Markdown output %q; body: %s", unsafe, body)
		}
	}
}

func TestTaskPageShowsOnlyRequestedTask(t *testing.T) {
	t.Parallel()

	handler, service := testHandlerWithIDs(t, "first-task", "second-task")
	first, err := service.Create(context.Background(), task.CreateInput{Title: "First task"})
	if err != nil {
		t.Fatalf("create first task: %v", err)
	}
	second, err := service.Create(context.Background(), task.CreateInput{Title: "Second task"})
	if err != nil {
		t.Fatalf("create second task: %v", err)
	}
	cookie, _ := login(t, handler)

	index := httptest.NewRecorder()
	indexRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	indexRequest.AddCookie(cookie)
	handler.ServeHTTP(index, indexRequest)
	for _, item := range []task.Task{first, second} {
		if want := `href="/` + item.ID + `"`; !strings.Contains(index.Body.String(), want) {
			t.Errorf("index does not link to task %q; body: %s", item.ID, index.Body.String())
		}
	}

	detail := httptest.NewRecorder()
	detailRequest := httptest.NewRequest(http.MethodGet, "/"+first.ID, nil)
	detailRequest.AddCookie(cookie)
	handler.ServeHTTP(detail, detailRequest)
	if detail.Code != http.StatusOK {
		t.Fatalf("task detail status = %d; body: %s", detail.Code, detail.Body.String())
	}
	if body := detail.Body.String(); !strings.Contains(body, first.Title) || strings.Contains(body, second.Title) {
		t.Fatalf("task detail does not isolate the requested task; body: %s", body)
	}
	if body := detail.Body.String(); !strings.Contains(body, `<a class="back-link" href="/">All tasks</a>`) {
		t.Errorf("task detail does not link back to all tasks; body: %s", body)
	}

	missing := httptest.NewRecorder()
	missingRequest := httptest.NewRequest(http.MethodGet, "/missing-task", nil)
	missingRequest.AddCookie(cookie)
	handler.ServeHTTP(missing, missingRequest)
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing task status = %d, want %d; body: %s", missing.Code, http.StatusNotFound, missing.Body.String())
	}
}

func TestIndexRendersTasksAsKanbanBoard(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := tasktest.NewRepository()
	now := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	for _, item := range []task.Task{
		{ID: "plan-launch", Title: "Plan the launch", Status: task.StatusTodo, CreatedAt: now, UpdatedAt: now, Version: 1},
		{ID: "build-launch", Title: "Build the launch", Status: task.StatusInProgress, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Version: 2},
		{ID: "write-brief", Title: "Write the brief", Status: task.StatusDone, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now, Version: 2},
	} {
		if err := repository.Create(context.Background(), item); err != nil {
			t.Fatalf("seed task %q: %v", item.ID, err)
		}
	}
	service := task.NewService(repository, time.Now, func() string { return "new-task" })
	handler := New(Config{
		Tasks:   service,
		Reader:  repository,
		Objects: objectstore.NewLocal(filepath.Join(root, "objects")),
		Auth:    auth.NewServer(auth.Config{Issuer: "http://tasks.example.com", Secret: "shared-secret"}),
	})
	cookie, _ := login(t, handler)

	page := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	handler.ServeHTTP(page, request)
	if page.Code != http.StatusOK {
		t.Fatalf("index status = %d; body: %s", page.Code, page.Body.String())
	}

	body := page.Body.String()
	if !strings.Contains(body, `class="kanban-board"`) {
		t.Fatalf("index does not render a kanban board; body: %s", body)
	}
	todoColumn := findKanbanColumn(t, body, task.StatusTodo)
	inProgressColumn := findKanbanColumn(t, body, task.StatusInProgress)
	doneColumn := findKanbanColumn(t, body, task.StatusDone)
	if !strings.Contains(todoColumn, "To do") || !strings.Contains(todoColumn, `class="column-count">1</span>`) {
		t.Fatalf("todo column heading or count is missing: %s", todoColumn)
	}
	if !strings.Contains(inProgressColumn, "In progress") || !strings.Contains(inProgressColumn, `class="column-count">1</span>`) {
		t.Fatalf("in-progress column heading or count is missing: %s", inProgressColumn)
	}
	if !strings.Contains(doneColumn, "Done") || !strings.Contains(doneColumn, `class="column-count">1</span>`) {
		t.Fatalf("done column heading or count is missing: %s", doneColumn)
	}
	if !strings.Contains(todoColumn, "Plan the launch") || strings.Contains(todoColumn, "Build the launch") || strings.Contains(todoColumn, "Write the brief") {
		t.Fatalf("todo column contains the wrong tasks: %s", todoColumn)
	}
	if !strings.Contains(inProgressColumn, "Build the launch") || strings.Contains(inProgressColumn, "Plan the launch") || strings.Contains(inProgressColumn, "Write the brief") {
		t.Fatalf("in-progress column contains the wrong tasks: %s", inProgressColumn)
	}
	if !strings.Contains(doneColumn, "Write the brief") || strings.Contains(doneColumn, "Plan the launch") || strings.Contains(doneColumn, "Build the launch") {
		t.Fatalf("done column contains the wrong tasks: %s", doneColumn)
	}
	if !strings.Contains(todoColumn, `/tasks/plan-launch/start`) || !strings.Contains(inProgressColumn, `/tasks/build-launch/done`) || strings.Contains(doneColumn, `/tasks/write-brief/`) {
		t.Fatalf("workflow controls do not match task state; todo: %s; in progress: %s; done: %s", todoColumn, inProgressColumn, doneColumn)
	}
}

func TestCreateCompleteAndUploadImage(t *testing.T) {
	t.Parallel()

	handler, service := testHandler(t)
	cookie, csrf := login(t, handler)

	create := postForm(handler, "/tasks", url.Values{
		"csrf":        {csrf},
		"title":       {"Ship the web UI"},
		"description": {"Exercise shared backend behavior."},
	}, cookie)
	if create.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	items, err := service.List(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("List = %#v, %v", items, err)
	}
	created := items[0]

	start := postForm(handler, "/tasks/"+created.ID+"/start", url.Values{"csrf": {csrf}}, cookie)
	if start.Code != http.StatusSeeOther {
		t.Fatalf("start status = %d; body: %s", start.Code, start.Body.String())
	}
	started, err := service.Get(context.Background(), created.ID)
	if err != nil || started.Status != task.StatusInProgress {
		t.Fatalf("started task = %#v, %v", started, err)
	}

	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatalf("write csrf: %v", err)
	}
	file, err := writer.CreateFormFile("image", "pixel.png")
	if err != nil {
		t.Fatalf("create image form part: %v", err)
	}
	if _, err := file.Write([]byte("\x89PNG\r\n\x1a\nimage")); err != nil {
		t.Fatalf("write image: %v", err)
	}
	writer.Close()
	uploadRequest := httptest.NewRequest(http.MethodPost, "/tasks/"+created.ID+"/images", &uploadBody)
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRequest.AddCookie(cookie)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d; body: %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	withImage, err := service.Get(context.Background(), created.ID)
	if err != nil || len(withImage.Attachments) != 1 || withImage.Attachments[0].ContentType != "image/png" {
		t.Fatalf("task after upload = %#v, %v", withImage, err)
	}

	complete := postForm(handler, "/tasks/"+created.ID+"/done", url.Values{"csrf": {csrf}}, cookie)
	if complete.Code != http.StatusSeeOther {
		t.Fatalf("complete status = %d; body: %s", complete.Code, complete.Body.String())
	}
	completed, err := service.Get(context.Background(), created.ID)
	if err != nil || completed.Status != task.StatusDone {
		t.Fatalf("completed task = %#v, %v", completed, err)
	}
}

func TestTaskMutationsCarryWebAuditAttribution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repository := &auditCapturingRepository{Repository: tasktest.NewRepository()}
	service := task.NewService(repository, time.Now, func() string { return "web-audit" })
	handler := New(Config{
		Tasks:   service,
		Reader:  repository,
		Objects: objectstore.NewLocal(filepath.Join(root, "objects")),
		Auth:    auth.NewServer(auth.Config{Issuer: "http://tasks.example.com", Secret: "shared-secret"}),
	})
	cookie, csrf := login(t, handler)
	create := postForm(handler, "/tasks", url.Values{
		"csrf": {csrf}, "title": {"Web audit"},
	}, cookie)
	if create.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d; body: %s", create.Code, create.Body.String())
	}
	start := postForm(handler, "/tasks/web-audit/start", url.Values{"csrf": {csrf}}, cookie)
	if start.Code != http.StatusSeeOther {
		t.Fatalf("start status = %d; body: %s", start.Code, start.Body.String())
	}
	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatalf("write csrf: %v", err)
	}
	file, err := writer.CreateFormFile("image", "audit.png")
	if err != nil {
		t.Fatalf("create image form part: %v", err)
	}
	if _, err := file.Write([]byte("\x89PNG\r\n\x1a\nimage")); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	uploadRequest := httptest.NewRequest(http.MethodPost, "/tasks/web-audit/images", &uploadBody)
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRequest.AddCookie(cookie)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d; body: %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	complete := postForm(handler, "/tasks/web-audit/done", url.Values{"csrf": {csrf}}, cookie)
	if complete.Code != http.StatusSeeOther {
		t.Fatalf("complete status = %d; body: %s", complete.Code, complete.Body.String())
	}

	if len(repository.mutations) != 4 {
		t.Fatalf("captured mutations = %#v", repository.mutations)
	}
	wantActions := []string{"create", "start", "add_attachment", "complete"}
	for index, mutation := range repository.mutations {
		if mutation.Action != wantActions[index] || mutation.ActorKind != "shared_secret" || mutation.Source != "web" {
			t.Fatalf("mutation %d attribution = %#v", index, mutation)
		}
	}
}

type auditCapturingRepository struct {
	*tasktest.Repository
	mutations []task.AuditMetadata
}

func (r *auditCapturingRepository) Create(ctx context.Context, item task.Task) error {
	r.mutations = append(r.mutations, task.AuditMetadataFromContext(ctx))
	return r.Repository.Create(ctx, item)
}

func (r *auditCapturingRepository) Update(ctx context.Context, item task.Task) error {
	r.mutations = append(r.mutations, task.AuditMetadataFromContext(ctx))
	return r.Repository.Update(ctx, item)
}

func testHandler(t *testing.T) (http.Handler, *task.Service) {
	t.Helper()
	return testHandlerWithIDs(t, strings.ToLower(t.Name())+"-id")
}

func testHandlerWithIDs(t *testing.T, ids ...string) (http.Handler, *task.Service) {
	t.Helper()
	root := t.TempDir()
	repo := tasktest.NewRepository()
	nextID := 0
	service := task.NewService(repo, time.Now, func() string {
		if nextID >= len(ids) {
			t.Fatalf("test requested more than %d task IDs", len(ids))
		}
		id := ids[nextID]
		nextID++
		return id
	})
	handler := New(Config{
		Tasks:   service,
		Reader:  repo,
		Objects: objectstore.NewLocal(filepath.Join(root, "objects")),
		Auth:    auth.NewServer(auth.Config{Issuer: "http://tasks.example.com", Secret: "shared-secret"}),
	})
	return handler, service
}

func login(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	response := postForm(handler, "/login", url.Values{"secret": {"shared-secret"}}, nil)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d; body: %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %#v", cookies)
	}
	page := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookies[0])
	handler.ServeHTTP(page, request)
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatalf("CSRF token not found in page: %s", page.Body.String())
	}
	return cookies[0], match[1]
}

func postForm(handler http.Handler, target string, values url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func findKanbanColumn(t *testing.T, body string, status task.Status) string {
	t.Helper()
	pattern := `(?s)<section class="kanban-column" data-status="` + regexp.QuoteMeta(string(status)) + `".*?</section>`
	column := regexp.MustCompile(pattern).FindString(body)
	if column == "" {
		t.Fatalf("kanban column %q not found in body: %s", status, body)
	}
	return column
}
