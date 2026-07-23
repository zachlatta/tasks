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

	if len(repository.mutations) != 3 {
		t.Fatalf("captured mutations = %#v", repository.mutations)
	}
	wantActions := []string{"create", "add_attachment", "complete"}
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
	root := t.TempDir()
	repo := tasktest.NewRepository()
	service := task.NewService(repo, time.Now, func() string { return strings.ToLower(t.Name()) + "-id" })
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
