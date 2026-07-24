package web

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
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

func TestAuthenticatedSessionNeverExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	root := t.TempDir()
	repo := tasktest.NewRepository()
	service := task.NewService(repo, clock, func() string { return "durable-id" })
	handler := New(Config{
		Tasks:   service,
		Reader:  repo,
		Objects: objectstore.NewLocal(filepath.Join(root, "objects")),
		Auth:    auth.NewServer(auth.Config{Issuer: "http://tasks.example.com", Secret: "shared-secret"}),
		Now:     func() time.Time { return clock() },
	})

	response := postForm(handler, "/login", url.Values{"secret": {"shared-secret"}}, nil)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d; body: %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %#v", cookies)
	}
	cookie := cookies[0]
	// The login cookie must persist across browser restarts, not expire when the
	// browser closes.
	if cookie.MaxAge <= 0 {
		t.Fatalf("session cookie is not persistent: MaxAge = %d", cookie.MaxAge)
	}

	// A decade later, with no activity in between, the session is still valid.
	now = now.AddDate(10, 0, 0)
	page := get(t, handler, "/", cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("session expired for an authed client: status = %d, location = %q", page.Code, page.Header().Get("Location"))
	}
	// Loading a page slides the cookie forward so an active client is never
	// dropped by the browser's cookie-lifetime cap.
	var refreshed *http.Cookie
	for _, c := range page.Result().Cookies() {
		if c.Name == sessionCookie {
			refreshed = c
		}
	}
	if refreshed == nil || refreshed.MaxAge <= 0 || refreshed.Value != cookie.Value {
		t.Fatalf("page load did not slide the session cookie forward: %#v", refreshed)
	}
}

func TestLegacySessionWithExpiryStillExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	repo := tasktest.NewRepository()
	service := task.NewService(repo, func() time.Time { return now }, func() string { return "legacy-id" })
	sessions := newMemorySessionStore()
	// A session issued before infinite sessions carries a real expiry that has
	// already passed; it must not authenticate.
	if err := sessions.SaveSession(context.Background(), hashToken("legacy-token"), "csrf", now.Add(-time.Minute)); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}
	handler := New(Config{
		Tasks:    service,
		Reader:   repo,
		Objects:  objectstore.NewLocal(filepath.Join(root, "objects")),
		Auth:     auth.NewServer(auth.Config{Issuer: "http://tasks.example.com", Secret: "shared-secret"}),
		Now:      func() time.Time { return now },
		Sessions: sessions,
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "legacy-token"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/login" {
		t.Fatalf("expired legacy session still authenticates: status = %d, location = %q", response.Code, response.Header().Get("Location"))
	}
}

func TestLogoutEndsSession(t *testing.T) {
	t.Parallel()

	handler, _ := testHandler(t)
	cookie, csrf := login(t, handler)

	logout := postForm(handler, "/logout", url.Values{"csrf": {csrf}}, cookie)
	if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/login" {
		t.Fatalf("logout response = %d %q", logout.Code, logout.Header().Get("Location"))
	}
	var cleared *http.Cookie
	for _, c := range logout.Result().Cookies() {
		if c.Name == sessionCookie {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("logout did not clear the session cookie: %#v", cleared)
	}
	// The server-side session is gone, so the old cookie no longer authenticates.
	page := get(t, handler, "/", cookie)
	if page.Code != http.StatusSeeOther || page.Header().Get("Location") != "/login" {
		t.Fatalf("session still valid after logout: status = %d, location = %q", page.Code, page.Header().Get("Location"))
	}
}

func TestBoardShowsLogoutButton(t *testing.T) {
	t.Parallel()

	handler, _ := testHandler(t)
	cookie, _ := login(t, handler)

	body := get(t, handler, "/", cookie).Body.String()
	if !strings.Contains(body, `action="/logout"`) || !strings.Contains(body, "Sign out") {
		t.Fatalf("board is missing a logout button; body: %s", body)
	}
}

func TestTaskDescriptionsRenderSafeMarkdown(t *testing.T) {
	t.Parallel()

	handler, service := testHandler(t)
	created, err := service.Create(context.Background(), task.CreateInput{
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

	detail := get(t, handler, "/"+created.ID, cookie)
	body := detail.Body.String()
	for _, want := range []string{
		`<div class="description">`,
		`<h2>Launch plan</h2>`,
		`<li>render <strong>bold</strong> text</li>`,
		`href="https://example.com"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("task detail does not contain rendered Markdown %q; body: %s", want, body)
		}
	}
	for _, unsafe := range []string{`<script>`, `href="javascript:`} {
		if strings.Contains(body, unsafe) {
			t.Errorf("task detail contains unsafe Markdown output %q; body: %s", unsafe, body)
		}
	}

	// The board shows a plain-text preview instead of the full description.
	board := get(t, handler, "/", cookie).Body.String()
	if !strings.Contains(board, "Launch plan render bold text") {
		t.Errorf("board card does not show a description preview; body: %s", board)
	}
	for _, unwanted := range []string{`<h2>Launch plan</h2>`, `<div class="description">`} {
		if strings.Contains(board, unwanted) {
			t.Errorf("board card renders full description markup %q; body: %s", unwanted, board)
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

	handler, repository := boardHandler(t)
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
	cookie, _ := login(t, handler)

	page := get(t, handler, "/", cookie)
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
	if !strings.Contains(inProgressColumn, "Build the launch") || strings.Contains(inProgressColumn, "Plan the launch") {
		t.Fatalf("in-progress column contains the wrong tasks: %s", inProgressColumn)
	}
	if !strings.Contains(doneColumn, "Write the brief") || strings.Contains(doneColumn, "Plan the launch") {
		t.Fatalf("done column contains the wrong tasks: %s", doneColumn)
	}

	// Every column is a drop target and every card can be picked up and opened.
	for _, column := range []string{todoColumn, inProgressColumn, doneColumn} {
		if !strings.Contains(column, `class="column-task-list"`) || !strings.Contains(column, `data-dropzone`) {
			t.Fatalf("column is not a drop target: %s", column)
		}
	}
	if !strings.Contains(todoColumn, `data-task-id="plan-launch"`) || !strings.Contains(todoColumn, `draggable="true"`) {
		t.Fatalf("todo card is not draggable: %s", todoColumn)
	}
	if !strings.Contains(todoColumn, `href="/plan-launch"`) {
		t.Fatalf("todo card does not link to its detail page: %s", todoColumn)
	}
	// Cards are previews: the board offers moves, not the full detail controls.
	if !strings.Contains(todoColumn, `action="/tasks/plan-launch/move"`) {
		t.Fatalf("todo card has no move control: %s", todoColumn)
	}
	if strings.Contains(body, `enctype="multipart/form-data"`) {
		t.Fatalf("board renders detail-only upload controls; body: %s", body)
	}
}

func TestMoveTaskAcrossAndWithinColumns(t *testing.T) {
	t.Parallel()

	handler, service := testHandlerWithIDs(t, "first", "second", "third")
	for _, title := range []string{"First", "Second", "Third"} {
		if _, err := service.Create(context.Background(), task.CreateInput{Title: title}); err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
	}
	cookie, csrf := login(t, handler)

	move := postForm(handler, "/tasks/second/move", url.Values{
		"csrf": {csrf}, "status": {string(task.StatusInProgress)}, "index": {"0"},
	}, cookie)
	if move.Code != http.StatusSeeOther {
		t.Fatalf("move status = %d; body: %s", move.Code, move.Body.String())
	}
	moved, err := service.Get(context.Background(), "second")
	if err != nil || moved.Status != task.StatusInProgress {
		t.Fatalf("moved task = %#v, %v", moved, err)
	}

	// Reordering inside a column is the same request with a different index.
	if response := postForm(handler, "/tasks/first/move", url.Values{
		"csrf": {csrf}, "status": {string(task.StatusTodo)}, "index": {"0"},
	}, cookie); response.Code != http.StatusSeeOther {
		t.Fatalf("reorder status = %d; body: %s", response.Code, response.Body.String())
	}
	if got, want := boardOrder(t, service, task.StatusTodo), []string{"first", "third"}; !slices.Equal(got, want) {
		t.Fatalf("todo column = %v, want %v", got, want)
	}

	// Done work can be dragged back out again.
	if response := postForm(handler, "/tasks/second/move", url.Values{
		"csrf": {csrf}, "status": {string(task.StatusDone)},
	}, cookie); response.Code != http.StatusSeeOther {
		t.Fatalf("complete status = %d; body: %s", response.Code, response.Body.String())
	}
	if response := postForm(handler, "/tasks/second/move", url.Values{
		"csrf": {csrf}, "status": {string(task.StatusTodo)}, "index": {"1"},
	}, cookie); response.Code != http.StatusSeeOther {
		t.Fatalf("reopen status = %d; body: %s", response.Code, response.Body.String())
	}
	if got, want := boardOrder(t, service, task.StatusTodo), []string{"first", "second", "third"}; !slices.Equal(got, want) {
		t.Fatalf("todo column after reopen = %v, want %v", got, want)
	}
}

func TestMoveTaskAnswersDragAndDropWithJSON(t *testing.T) {
	t.Parallel()

	handler, service := testHandlerWithIDs(t, "drag-me")
	if _, err := service.Create(context.Background(), task.CreateInput{Title: "Drag me"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	cookie, csrf := login(t, handler)

	request := httptest.NewRequest(http.MethodPost, "/tasks/drag-me/move",
		strings.NewReader(url.Values{"csrf": {csrf}, "status": {"in_progress"}, "index": {"0"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("json move status = %d; body: %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("json move content type = %q", contentType)
	}
	var payload struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Card   string `json:"card"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode json move response: %v; body: %s", err, response.Body.String())
	}
	if payload.ID != "drag-me" || payload.Status != "in_progress" {
		t.Fatalf("json move payload = %#v", payload)
	}
	// The refreshed card lets the board swap in server-rendered markup.
	if !strings.Contains(payload.Card, `data-task-id="drag-me"`) || !strings.Contains(payload.Card, "Drag me") {
		t.Fatalf("json move card = %q", payload.Card)
	}
}

func TestMoveTaskRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	handler, service := testHandlerWithIDs(t, "prerequisite", "blocked")
	if _, err := service.Create(context.Background(), task.CreateInput{Title: "Prerequisite"}); err != nil {
		t.Fatalf("create prerequisite: %v", err)
	}
	if _, err := service.Create(context.Background(), task.CreateInput{
		Title: "Blocked", Dependencies: []string{"prerequisite"},
	}); err != nil {
		t.Fatalf("create blocked task: %v", err)
	}
	cookie, csrf := login(t, handler)

	for name, expectation := range map[string]struct {
		values url.Values
		status int
	}{
		"missing csrf":   {url.Values{"status": {"done"}}, http.StatusForbidden},
		"unknown status": {url.Values{"csrf": {csrf}, "status": {"archived"}}, http.StatusBadRequest},
		"blocked":        {url.Values{"csrf": {csrf}, "status": {"done"}}, http.StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			response := postForm(handler, "/tasks/blocked/move", expectation.values, cookie)
			if response.Code != expectation.status {
				t.Fatalf("status = %d, want %d; body: %s", response.Code, expectation.status, response.Body.String())
			}
		})
	}
	unchanged, err := service.Get(context.Background(), "blocked")
	if err != nil || unchanged.Status != task.StatusTodo {
		t.Fatalf("task after rejected moves = %#v, %v", unchanged, err)
	}
}

func TestTaskDetailServesADrawerFragment(t *testing.T) {
	t.Parallel()

	handler, service := testHandlerWithIDs(t, "detail-me")
	if _, err := service.Create(context.Background(), task.CreateInput{
		Title: "Detail me", Description: "Full **detail** here.",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	cookie, _ := login(t, handler)

	page := get(t, handler, "/detail-me", cookie)
	if body := page.Body.String(); !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "<strong>detail</strong>") {
		t.Fatalf("detail page is not a full page with rendered Markdown; body: %s", body)
	}
	fragment := get(t, handler, "/detail-me?partial=1", cookie)
	if fragment.Code != http.StatusOK {
		t.Fatalf("fragment status = %d; body: %s", fragment.Code, fragment.Body.String())
	}
	body := fragment.Body.String()
	if strings.Contains(body, "<!doctype html>") || strings.Contains(body, "<body") {
		t.Fatalf("fragment is a full page; body: %s", body)
	}
	if !strings.Contains(body, `data-task-id="detail-me"`) || !strings.Contains(body, "<strong>detail</strong>") {
		t.Fatalf("fragment does not contain the task detail; body: %s", body)
	}
	if !strings.Contains(body, `enctype="multipart/form-data"`) {
		t.Fatalf("fragment does not offer image upload; body: %s", body)
	}
}

func TestBoardScriptAndStylesAreServed(t *testing.T) {
	t.Parallel()

	handler, _ := testHandler(t)
	for path, wantType := range map[string]string{
		"/static/app.css": "text/css",
		"/static/app.js":  "text/javascript",
	} {
		response := get(t, handler, path, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
		if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, wantType) {
			t.Fatalf("%s content type = %q, want %q", path, contentType, wantType)
		}
		if response.Body.Len() == 0 {
			t.Fatalf("%s served an empty body", path)
		}
	}
}

func TestCreateMoveAndUploadFile(t *testing.T) {
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

	start := postForm(handler, "/tasks/"+created.ID+"/move", url.Values{
		"csrf": {csrf}, "status": {string(task.StatusInProgress)},
	}, cookie)
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
	file, err := writer.CreateFormFile("file", "release-notes.pdf")
	if err != nil {
		t.Fatalf("create file form part: %v", err)
	}
	contents := []byte("%PDF-1.7\nrelease notes")
	if _, err := file.Write(contents); err != nil {
		t.Fatalf("write file: %v", err)
	}
	writer.Close()
	uploadRequest := httptest.NewRequest(http.MethodPost, "/tasks/"+created.ID+"/attachments", &uploadBody)
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRequest.AddCookie(cookie)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d; body: %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	withFile, err := service.Get(context.Background(), created.ID)
	if err != nil || len(withFile.Attachments) != 1 {
		t.Fatalf("task after upload = %#v, %v", withFile, err)
	}
	attachment := withFile.Attachments[0]
	if attachment.Name != "release-notes.pdf" || attachment.ContentType != "application/pdf" {
		t.Fatalf("attachment = %#v", attachment)
	}

	download := get(t, handler, "/attachments/"+attachment.Key, cookie)
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d; body: %s", download.Code, download.Body.String())
	}
	if got := download.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("download content type = %q, want application/pdf", got)
	}
	if got := download.Header().Get("Content-Disposition"); got != "attachment" {
		t.Errorf("download content disposition = %q, want attachment", got)
	}
	if !bytes.Equal(download.Body.Bytes(), contents) {
		t.Errorf("download contents = %q, want %q", download.Body.Bytes(), contents)
	}

	detail := get(t, handler, "/"+created.ID, cookie).Body.String()
	if !strings.Contains(detail, `href="/attachments/`+attachment.Key+`"`) ||
		!strings.Contains(detail, `download="release-notes.pdf"`) {
		t.Errorf("task detail does not render a downloadable file attachment; body: %s", detail)
	}
	if !strings.Contains(detail, "Attach a file") || strings.Contains(detail, `accept="image/*"`) {
		t.Errorf("task detail still restricts uploads to images; body: %s", detail)
	}

	complete := postForm(handler, "/tasks/"+created.ID+"/move", url.Values{
		"csrf": {csrf}, "status": {string(task.StatusDone)},
	}, cookie)
	if complete.Code != http.StatusSeeOther {
		t.Fatalf("complete status = %d; body: %s", complete.Code, complete.Body.String())
	}
	completed, err := service.Get(context.Background(), created.ID)
	if err != nil || completed.Status != task.StatusDone {
		t.Fatalf("completed task = %#v, %v", completed, err)
	}
}

func TestUploadAllowsFilesLargerThanTenMiB(t *testing.T) {
	t.Parallel()

	handler, service := testHandler(t)
	created, err := service.Create(context.Background(), task.CreateInput{Title: "Upload a large file"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	cookie, csrf := login(t, handler)

	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatalf("write csrf: %v", err)
	}
	file, err := writer.CreateFormFile("file", "eleven-megabytes.bin")
	if err != nil {
		t.Fatalf("create file form part: %v", err)
	}
	oneMiB := make([]byte, 1<<20)
	for range 11 {
		if _, err := file.Write(oneMiB); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/tasks/"+created.ID+"/attachments", &uploadBody)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d; body: %s", response.Code, response.Body.String())
	}
}

func TestFileUploadAnswersTheDrawerWithARefreshedCard(t *testing.T) {
	t.Parallel()

	handler, service := testHandlerWithIDs(t, "shoot-me")
	if _, err := service.Create(context.Background(), task.CreateInput{Title: "Shoot me"}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	cookie, csrf := login(t, handler)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatalf("write csrf: %v", err)
	}
	file, err := writer.CreateFormFile("file", "shot.png")
	if err != nil {
		t.Fatalf("create image part: %v", err)
	}
	if _, err := file.Write([]byte("\x89PNG\r\n\x1a\nimage")); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/tasks/shoot-me/attachments", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept", "application/json")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status = %d; body: %s", response.Code, response.Body.String())
	}
	var payload struct {
		ID   string `json:"id"`
		Card string `json:"card"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode upload response: %v; body: %s", err, response.Body.String())
	}
	// The refreshed card carries the new image count and cover thumbnail.
	if payload.ID != "shoot-me" || !strings.Contains(payload.Card, "1 file") || !strings.Contains(payload.Card, "card-cover") {
		t.Fatalf("upload response card = %q", payload.Card)
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
	start := postForm(handler, "/tasks/web-audit/move", url.Values{
		"csrf": {csrf}, "status": {string(task.StatusInProgress)},
	}, cookie)
	if start.Code != http.StatusSeeOther {
		t.Fatalf("start status = %d; body: %s", start.Code, start.Body.String())
	}
	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatalf("write csrf: %v", err)
	}
	file, err := writer.CreateFormFile("file", "audit.png")
	if err != nil {
		t.Fatalf("create image form part: %v", err)
	}
	if _, err := file.Write([]byte("\x89PNG\r\n\x1a\nimage")); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	uploadRequest := httptest.NewRequest(http.MethodPost, "/tasks/web-audit/attachments", &uploadBody)
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRequest.AddCookie(cookie)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d; body: %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	complete := postForm(handler, "/tasks/web-audit/move", url.Values{
		"csrf": {csrf}, "status": {string(task.StatusDone)},
	}, cookie)
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

// boardHandler returns a handler over a repository tests can seed directly,
// for board layout assertions that do not go through the service.
func boardHandler(t *testing.T) (http.Handler, *tasktest.Repository) {
	t.Helper()
	root := t.TempDir()
	repository := tasktest.NewRepository()
	handler := New(Config{
		Tasks:   task.NewService(repository, time.Now, func() string { return "new-task" }),
		Reader:  repository,
		Objects: objectstore.NewLocal(filepath.Join(root, "objects")),
		Auth:    auth.NewServer(auth.Config{Issuer: "http://tasks.example.com", Secret: "shared-secret"}),
	})
	return handler, repository
}

func boardOrder(t *testing.T, service *task.Service, status task.Status) []string {
	t.Helper()
	items, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Status == status {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func get(t *testing.T, handler http.Handler, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
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

func TestExcerptFlattensMarkdownForCards(t *testing.T) {
	t.Parallel()

	for name, expectation := range map[string]struct{ source, want string }{
		"headings and emphasis": {"## Plan\n\nShip **fast**", "Plan Ship fast"},
		"task list":             {"- [x] first step\n- [ ] second step", "first step second step"},
		"links":                 {"see [the docs](https://example.com)", "see the docs"},
		"images":                {"![a diagram](/images/x.png) after", "after"},
		"fenced code":           {"before\n\n```go\nfmt.Println(1)\n```\n\nafter", "before after"},
		"raw html":              {"<script>alert(1)</script>keep", "alert(1) keep"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := excerpt(expectation.source); got != expectation.want {
				t.Fatalf("excerpt(%q) = %q, want %q", expectation.source, got, expectation.want)
			}
		})
	}

	long := strings.Repeat("word ", 100)
	shortened := excerpt(long)
	if !strings.HasSuffix(shortened, "…") || len([]rune(shortened)) > excerptLimit+1 {
		t.Fatalf("excerpt did not truncate a long description: %q", shortened)
	}
}
