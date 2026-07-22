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

	"github.com/zachlatta/task-tracker/internal/auth"
	"github.com/zachlatta/task-tracker/internal/objectstore"
	"github.com/zachlatta/task-tracker/internal/task"
	"github.com/zachlatta/task-tracker/internal/tasktest"
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
	page := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(login.Result().Cookies()[0])
	handler.ServeHTTP(page, request)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), created.Title) {
		t.Fatalf("task page status = %d; body: %s", page.Code, page.Body.String())
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
